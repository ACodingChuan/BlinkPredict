package chainconfirm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"blinkpredict/banckend/internal/logging"

	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

const (
	defaultRouterShards      = 2
	defaultTerminalCacheTTL  = 60 * time.Second
	defaultReconnectBaseWait = 500 * time.Millisecond
	defaultReconnectMaxWait  = 8 * time.Second
	defaultPingInterval      = 20 * time.Second
	defaultPongWait          = 60 * time.Second
)

type SignatureResult struct {
	Signature          string
	Slot               uint64
	ConfirmationStatus string
	ErrText            string
	ObservedAt         time.Time
}

type WSRouter interface {
	SubscribeSignature(signature string, subscriberID string, kind string, commitment string, ch chan<- SignatureResult) (func(), error)
	Close()
}

type subscriptionKey struct {
	Signature  string
	Commitment string
}

type Router struct {
	log      string
	wsURL    string
	ttl      time.Duration
	reqID    atomic.Uint64
	closed   atomic.Bool
	mu       sync.Mutex
	watches  map[subscriptionKey]*signatureWatch
	shards   []*routerShard
	dialer   *websocket.Dialer
	stopOnce sync.Once
}

type signatureWatch struct {
	Key           subscriptionKey
	Signature     string
	Commitment    string
	ShardIndex    int
	WSSubID       uint64
	PendingReqID  uint64
	Subscribers   map[string]signatureSubscriber
	TerminalCache *SignatureResult
	TerminalUntil time.Time
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

type signatureSubscriber struct {
	ID   string
	Kind string
	Ch   chan<- SignatureResult
}

type routerShard struct {
	router *Router
	index  int
	url    string

	wakeCh chan struct{}

	stateMu         sync.RWMutex
	conn            *websocket.Conn
	connected       bool
	pendingRequests map[uint64]subscriptionKey
	sigByWSSubID    map[uint64]subscriptionKey

	writeMu sync.Mutex
}

type wsRPCResponse struct {
	ID     *uint64           `json:"id,omitempty"`
	Result json.RawMessage   `json:"result,omitempty"`
	Error  *wsRPCError       `json:"error,omitempty"`
	Params *wsRPCEventParams `json:"params,omitempty"`
}

type wsRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type wsRPCEventParams struct {
	Subscription uint64            `json:"subscription"`
	Result       wsSignatureNotify `json:"result"`
}

type wsSignatureNotify struct {
	Context struct {
		Slot uint64 `json:"slot"`
	} `json:"context"`
	Value struct {
		Err interface{} `json:"err"`
	} `json:"value"`
}

func NewRouter(wsURL string, rpcURL string, shardCount int) *Router {
	wsURL = deriveWSURL(strings.TrimSpace(wsURL), strings.TrimSpace(rpcURL))
	if wsURL == "" {
		return nil
	}
	if shardCount <= 0 {
		shardCount = defaultRouterShards
	}
	r := &Router{
		log:     "wsrouter",
		wsURL:   wsURL,
		ttl:     defaultTerminalCacheTTL,
		watches: make(map[subscriptionKey]*signatureWatch),
		shards:  make([]*routerShard, 0, shardCount),
		dialer: &websocket.Dialer{
			Proxy:             http.ProxyFromEnvironment,
			HandshakeTimeout:  10 * time.Second,
			EnableCompression: true,
		},
	}
	for i := 0; i < shardCount; i++ {
		shard := &routerShard{
			router:          r,
			index:           i,
			url:             wsURL,
			wakeCh:          make(chan struct{}, 1),
			pendingRequests: make(map[uint64]subscriptionKey),
			sigByWSSubID:    make(map[uint64]subscriptionKey),
		}
		r.shards = append(r.shards, shard)
		go shard.run()
	}
	go r.cleanupLoop()
	return r
}

func (r *Router) SubscribeSignature(signature string, subscriberID string, kind string, commitment string, ch chan<- SignatureResult) (func(), error) {
	if r == nil {
		return func() {}, fmt.Errorf("ws router is not configured")
	}
	signature = strings.TrimSpace(signature)
	subscriberID = strings.TrimSpace(subscriberID)
	kind = strings.TrimSpace(kind)
	commitment = normalizeCommitment(commitment)
	if signature == "" {
		return func() {}, fmt.Errorf("signature is required")
	}
	if subscriberID == "" {
		subscriberID = fmt.Sprintf("%s:%d", kind, time.Now().UnixNano())
	}
	key := subscriptionKey{Signature: signature, Commitment: commitment}
	var (
		shardIndex   int
		needUpstream bool
		cached       *SignatureResult
	)
	r.mu.Lock()
	watch, ok := r.watches[key]
	if !ok {
		shardIndex = shardIndexForSignature(signature+":"+commitment, len(r.shards))
		watch = &signatureWatch{
			Key:         key,
			Signature:   signature,
			Commitment:  commitment,
			ShardIndex:  shardIndex,
			Subscribers: make(map[string]signatureSubscriber),
			CreatedAt:   time.Now().UTC(),
			UpdatedAt:   time.Now().UTC(),
		}
		r.watches[key] = watch
		needUpstream = true
	} else {
		shardIndex = watch.ShardIndex
		if watch.TerminalCache != nil && time.Now().UTC().Before(watch.TerminalUntil) {
			c := *watch.TerminalCache
			cached = &c
		}
	}
	watch.Subscribers[subscriberID] = signatureSubscriber{ID: subscriberID, Kind: kind, Ch: ch}
	watch.UpdatedAt = time.Now().UTC()
	r.mu.Unlock()

	if cached != nil {
		select {
		case ch <- *cached:
		default:
			logging.LogWS(r.log, context.Background(), zerolog.WarnLevel, "ws router cached dispatch dropped", map[string]any{
				"signature":     signature,
				"commitment":    commitment,
				"subscriber_id": subscriberID,
				"kind":          kind,
			})
		}
		return func() { r.unsubscribe(key, subscriberID) }, nil
	}
	if needUpstream {
		r.shards[shardIndex].signal()
		if err := r.shards[shardIndex].sendSubscribe(key); err != nil {
			logging.LogWS(r.log, context.Background(), zerolog.WarnLevel, "ws router subscribe deferred to reconnect", map[string]any{
				"signature":  signature,
				"commitment": commitment,
				"shard":      shardIndex,
				"error":      err.Error(),
			})
		}
	}
	return func() { r.unsubscribe(key, subscriberID) }, nil
}

func (r *Router) unsubscribe(key subscriptionKey, subscriberID string) {
	if r == nil || r.closed.Load() {
		return
	}
	subscriberID = strings.TrimSpace(subscriberID)
	if key.Signature == "" || subscriberID == "" {
		return
	}
	var shard *routerShard
	var wsSubID uint64
	r.mu.Lock()
	watch, ok := r.watches[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	delete(watch.Subscribers, subscriberID)
	watch.UpdatedAt = time.Now().UTC()
	if len(watch.Subscribers) == 0 {
		shard = r.shards[watch.ShardIndex]
		wsSubID = watch.WSSubID
		watch.WSSubID = 0
		watch.PendingReqID = 0
		if watch.TerminalCache == nil || time.Now().UTC().After(watch.TerminalUntil) {
			delete(r.watches, key)
		}
	}
	r.mu.Unlock()
	if shard != nil && wsSubID != 0 {
		_ = shard.sendUnsubscribe(wsSubID)
	}
	if shard != nil {
		shard.signal()
		if !r.shardHasActiveSubscribers(shard.index) {
			shard.close()
		}
	}
}

func (r *Router) Close() {
	if r == nil {
		return
	}
	r.stopOnce.Do(func() {
		r.closed.Store(true)
		for _, shard := range r.shards {
			shard.signal()
			shard.close()
		}
	})
}

func (r *Router) cleanupLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for !r.closed.Load() {
		<-ticker.C
		now := time.Now().UTC()
		r.mu.Lock()
		for key, watch := range r.watches {
			if len(watch.Subscribers) > 0 {
				continue
			}
			if watch.TerminalCache != nil && now.Before(watch.TerminalUntil) {
				continue
			}
			delete(r.watches, key)
		}
		r.mu.Unlock()
	}
}

func (r *Router) cacheAndDispatch(key subscriptionKey, result SignatureResult) {
	var subscribers []signatureSubscriber
	r.mu.Lock()
	watch, ok := r.watches[key]
	if ok {
		result.ObservedAt = time.Now().UTC()
		cached := result
		watch.TerminalCache = &cached
		watch.TerminalUntil = time.Now().UTC().Add(r.ttl)
		watch.WSSubID = 0
		watch.PendingReqID = 0
		watch.UpdatedAt = time.Now().UTC()
		subscribers = make([]signatureSubscriber, 0, len(watch.Subscribers))
		for _, sub := range watch.Subscribers {
			subscribers = append(subscribers, sub)
		}
	}
	r.mu.Unlock()
	for _, sub := range subscribers {
		select {
		case sub.Ch <- result:
		default:
			logging.LogWS(r.log, context.Background(), zerolog.WarnLevel, "ws router dispatch dropped", map[string]any{
				"signature":     key.Signature,
				"commitment":    key.Commitment,
				"subscriber_id": sub.ID,
				"kind":          sub.Kind,
			})
		}
	}
}

func (r *Router) onSubscribeAck(reqID uint64, wsSubID uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, shard := range r.shards {
		key, ok := shard.pendingByRequest(reqID)
		if !ok {
			continue
		}
		shard.bindSubscription(reqID, wsSubID, key)
		if watch, exists := r.watches[key]; exists {
			watch.WSSubID = wsSubID
			watch.PendingReqID = 0
			watch.UpdatedAt = time.Now().UTC()
		}
		return
	}
}

func (r *Router) onNotification(subID uint64, notify wsSignatureNotify) {
	var key subscriptionKey
	found := false
	for _, shard := range r.shards {
		if k, ok := shard.subscriptionBySubID(subID); ok {
			key = k
			found = true
			break
		}
	}
	if !found {
		return
	}

	r.mu.Lock()
	watch, ok := r.watches[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	signature := watch.Signature
	commitment := watch.Commitment
	r.mu.Unlock()

	res := SignatureResult{
		Signature:          signature,
		Slot:               notify.Context.Slot,
		ConfirmationStatus: commitment,
	}
	if notify.Value.Err != nil {
		res.ErrText = fmt.Sprint(notify.Value.Err)
	}
	r.cacheAndDispatch(key, res)
}

func (r *Router) resetShard(index int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, watch := range r.watches {
		if watch.ShardIndex != index {
			continue
		}
		watch.WSSubID = 0
		watch.PendingReqID = 0
	}
}

func (r *Router) shardSubscriptions(index int) []subscriptionKey {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]subscriptionKey, 0)
	for key, watch := range r.watches {
		if watch.ShardIndex != index {
			continue
		}
		if len(watch.Subscribers) == 0 {
			continue
		}
		out = append(out, key)
	}
	return out
}

func (r *Router) shardHasActiveSubscribers(index int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, watch := range r.watches {
		if watch.ShardIndex != index {
			continue
		}
		if len(watch.Subscribers) > 0 {
			return true
		}
	}
	return false
}

func (r *Router) nextRequestID(key subscriptionKey, shardIndex int) uint64 {
	reqID := r.reqID.Add(1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if watch, ok := r.watches[key]; ok {
		watch.PendingReqID = reqID
		watch.ShardIndex = shardIndex
		watch.UpdatedAt = time.Now().UTC()
	}
	return reqID
}

func shardIndexForSignature(signature string, shardCount int) int {
	if shardCount <= 1 {
		return 0
	}
	var sum uint32
	for i := 0; i < len(signature); i++ {
		sum = sum*33 + uint32(signature[i])
	}
	return int(sum % uint32(shardCount))
}

func (s *routerShard) run() {
	backoff := defaultReconnectBaseWait
	for !s.router.closed.Load() {
		if !s.waitUntilNeeded() {
			return
		}
		if err := s.connect(); err != nil {
			logging.LogWS(s.router.log, context.Background(), zerolog.WarnLevel, "ws router connect failed", map[string]any{
				"shard": s.index,
				"error": err.Error(),
			})
			time.Sleep(backoff)
			if backoff < defaultReconnectMaxWait {
				backoff *= 2
				if backoff > defaultReconnectMaxWait {
					backoff = defaultReconnectMaxWait
				}
			}
			continue
		}
		backoff = defaultReconnectBaseWait
		s.resubscribeAll()
		pingStop := make(chan struct{})
		go s.pingLoop(pingStop)
		if err := s.readLoop(); err != nil && !s.router.closed.Load() && s.router.shardHasActiveSubscribers(s.index) {
			logging.LogWS(s.router.log, context.Background(), zerolog.WarnLevel, "ws router shard disconnected", map[string]any{
				"shard": s.index,
				"error": err.Error(),
			})
		}
		close(pingStop)
		s.router.resetShard(s.index)
		s.close()
	}
}

func (s *routerShard) waitUntilNeeded() bool {
	for !s.router.closed.Load() {
		if s.router.shardHasActiveSubscribers(s.index) {
			return true
		}
		s.close()
		<-s.wakeCh
	}
	return false
}

func (s *routerShard) connect() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	logging.LogWS(s.router.log, ctx, zerolog.InfoLevel, "connect solana websocket shard", map[string]any{
		"shard":  s.index,
		"ws_url": s.url,
	})
	conn, _, err := s.router.dialer.DialContext(ctx, s.url, nil)
	if err != nil {
		return err
	}
	_ = conn.SetReadDeadline(time.Now().Add(defaultPongWait))
	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(defaultPongWait))
	})
	s.stateMu.Lock()
	s.conn = conn
	s.connected = true
	s.pendingRequests = make(map[uint64]subscriptionKey)
	s.sigByWSSubID = make(map[uint64]subscriptionKey)
	s.stateMu.Unlock()
	return nil
}

func (s *routerShard) readLoop() error {
	for !s.router.closed.Load() {
		if !s.router.shardHasActiveSubscribers(s.index) {
			return nil
		}
		s.stateMu.RLock()
		conn := s.conn
		s.stateMu.RUnlock()
		if conn == nil {
			return errors.New("websocket connection is nil")
		}
		_, message, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var resp wsRPCResponse
		if err := json.Unmarshal(message, &resp); err != nil {
			logging.LogWS(s.router.log, context.Background(), zerolog.WarnLevel, "ws router decode failed", map[string]any{
				"shard":   s.index,
				"payload": string(message),
				"error":   err.Error(),
			})
			continue
		}
		if resp.ID != nil {
			if resp.Error != nil {
				logging.LogWS(s.router.log, context.Background(), zerolog.WarnLevel, "ws router subscription ack failed", map[string]any{
					"shard":  s.index,
					"req_id": *resp.ID,
					"error":  resp.Error.Message,
				})
				continue
			}
			var wsSubID uint64
			if err := json.Unmarshal(resp.Result, &wsSubID); err != nil {
				continue
			}
			s.router.onSubscribeAck(*resp.ID, wsSubID)
			continue
		}
		if resp.Params == nil {
			continue
		}
		s.router.onNotification(resp.Params.Subscription, resp.Params.Result)
	}
	return nil
}

func (s *routerShard) pingLoop(stop <-chan struct{}) {
	ticker := time.NewTicker(defaultPingInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			s.stateMu.RLock()
			conn := s.conn
			connected := s.connected
			s.stateMu.RUnlock()
			if !connected || conn == nil {
				return
			}
			s.writeMu.Lock()
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			err := conn.WriteMessage(websocket.PingMessage, nil)
			s.writeMu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

func (s *routerShard) resubscribeAll() {
	for _, key := range s.router.shardSubscriptions(s.index) {
		if err := s.sendSubscribe(key); err != nil {
			logging.LogWS(s.router.log, context.Background(), zerolog.WarnLevel, "ws router resubscribe failed", map[string]any{
				"shard":      s.index,
				"signature":  key.Signature,
				"commitment": key.Commitment,
				"error":      err.Error(),
			})
		}
	}
}

func (s *routerShard) sendSubscribe(key subscriptionKey) error {
	reqID := s.router.nextRequestID(key, s.index)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "signatureSubscribe",
		"params": []any{
			key.Signature,
			map[string]any{"commitment": key.Commitment},
		},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	s.pendingRequests[reqID] = key
	s.stateMu.Unlock()
	return s.writeJSON(data)
}

func (s *routerShard) sendUnsubscribe(wsSubID uint64) error {
	if wsSubID == 0 {
		return nil
	}
	reqID := s.router.reqID.Add(1)
	payload := map[string]any{
		"jsonrpc": "2.0",
		"id":      reqID,
		"method":  "signatureUnsubscribe",
		"params":  []any{wsSubID},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	s.stateMu.Lock()
	delete(s.sigByWSSubID, wsSubID)
	s.stateMu.Unlock()
	return s.writeJSON(data)
}

func (s *routerShard) writeJSON(data []byte) error {
	s.stateMu.RLock()
	conn := s.conn
	connected := s.connected
	s.stateMu.RUnlock()
	if !connected || conn == nil {
		return fmt.Errorf("websocket shard %d is disconnected", s.index)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return conn.WriteMessage(websocket.TextMessage, data)
}

func (s *routerShard) signal() {
	select {
	case s.wakeCh <- struct{}{}:
	default:
	}
}

func (s *routerShard) close() {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	if s.conn != nil {
		_ = s.conn.Close()
	}
	s.conn = nil
	s.connected = false
	s.pendingRequests = make(map[uint64]subscriptionKey)
	s.sigByWSSubID = make(map[uint64]subscriptionKey)
}

func (s *routerShard) pendingByRequest(reqID uint64) (subscriptionKey, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	key, ok := s.pendingRequests[reqID]
	return key, ok
}

func (s *routerShard) bindSubscription(reqID uint64, wsSubID uint64, key subscriptionKey) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	delete(s.pendingRequests, reqID)
	s.sigByWSSubID[wsSubID] = key
}

func (s *routerShard) subscriptionBySubID(subID uint64) (subscriptionKey, bool) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	key, ok := s.sigByWSSubID[subID]
	return key, ok
}

func normalizeCommitment(commitment string) string {
	switch strings.ToLower(strings.TrimSpace(commitment)) {
	case "processed":
		return "processed"
	case "confirmed":
		return "confirmed"
	case "finalized":
		return "finalized"
	default:
		return "confirmed"
	}
}
