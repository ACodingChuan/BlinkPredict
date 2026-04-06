package pusher

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/go-chi/chi/v5"
	"github.com/gorilla/websocket"
)

const (
	sendQueueSize = 64
	writeTimeout  = 10 * time.Second
	pingInterval  = 30 * time.Second
	pongTimeout   = 60 * time.Second
	authTimeout   = 5 * time.Second
)

type Hub struct {
	cfg        config.Config
	tickets    *TicketStore
	marketData MarketDataSource

	upgrader websocket.Upgrader

	mu          sync.RWMutex
	marketRooms map[uint64]map[*connection]struct{}
	marketState map[uint64]*marketState
	userRooms   map[string]map[*connection]struct{}
}

type connection struct {
	hub      *Hub
	ws       *websocket.Conn
	send     chan []byte
	marketID *uint64
	wallet   string
	remote   string

	bootMu        sync.Mutex
	bootstrapping bool
	pending       []queuedMarketMessage
}

type queuedMarketMessage struct {
	seq     uint64
	payload []byte
}

func NewHub(cfg config.Config, tickets *TicketStore, marketData MarketDataSource) *Hub {
	return &Hub{
		cfg:        cfg,
		tickets:    tickets,
		marketData: marketData,
		upgrader: websocket.Upgrader{
			ReadBufferSize:  1024,
			WriteBufferSize: 2048,
			CheckOrigin: func(r *http.Request) bool {
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				return origin == "http://localhost:3000" || origin == "http://127.0.0.1:3000"
			},
		},
		marketRooms: make(map[uint64]map[*connection]struct{}),
		marketState: make(map[uint64]*marketState),
		userRooms:   make(map[string]map[*connection]struct{}),
	}
}

func (h *Hub) ServeMarketWS(w http.ResponseWriter, r *http.Request) error {
	marketID, err := strconv.ParseUint(chi.URLParam(r, "marketId"), 10, 64)
	if err != nil {
		return err
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	client := &connection{
		hub:           h,
		ws:            ws,
		send:          make(chan []byte, sendQueueSize),
		marketID:      &marketID,
		remote:        r.RemoteAddr,
		bootstrapping: true,
		pending:       make([]queuedMarketMessage, 0, sendQueueSize),
	}
	h.registerMarket(client, marketID)
	logger.Infof("market ws connected market=%d remote=%s conns=%d", marketID, client.remote, h.marketConnectionCount(marketID))
	go client.writePump()
	snapshotBody, snapshotSeq, snapshotPayload, err := h.buildMarketSnapshot(r.Context(), marketID)
	if err != nil {
		h.unregister(client)
		_ = ws.Close()
		return err
	}
	client.finishMarketBootstrap(snapshotSeq, snapshotBody)
	logger.Infof(
		"market ws snapshot sent market=%d remote=%s seq=%d bids=%d asks=%d trades=%d price_points=%d",
		marketID,
		client.remote,
		snapshotSeq,
		len(snapshotPayload.Orderbook.Bids),
		len(snapshotPayload.Orderbook.Asks),
		len(snapshotPayload.Trades),
		len(snapshotPayload.PriceHistory),
	)
	client.readPump()
	return nil
}

func (h *Hub) ServeUserWS(w http.ResponseWriter, r *http.Request) error {
	wallet, err := h.consumeTicket(r.Context(), r.URL.Query().Get("ticket"))
	if err != nil {
		return err
	}
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	client := &connection{
		hub:  h,
		ws:   ws,
		send: make(chan []byte, sendQueueSize),
		remote: r.RemoteAddr,
	}
	h.registerUser(client, wallet)
	logger.Infof("user ws connected wallet=%s remote=%s conns=%d", wallet, client.remote, h.userConnectionCount(wallet))
	go client.writePump()
	client.readPump()
	return nil
}

func (h *Hub) PublishMarketDelta(ctx context.Context, event matching.MatchBatchEvent) error {
	state := h.marketStateFor(event.MarketID)
	state.ensureLoaded(ctx, event.MarketID, h.marketData)
	delta := state.applyMatchEvent(event)
	body, err := json.Marshal(delta)
	if err != nil {
		return err
	}
	seq, err := strconv.ParseUint(delta.Seq, 10, 64)
	if err != nil {
		return err
	}
	h.broadcastMarket(event.MarketID, seq, body)
	return nil
}

func (h *Hub) PublishUserOrder(payload protocol.UserOrderPush) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	h.broadcastUser(payload.WalletAddress, body)
	return nil
}

func (h *Hub) PublishUserMessage(wallet string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	h.broadcastUser(wallet, body)
	return nil
}

func (h *Hub) registerMarket(client *connection, marketID uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.marketRooms[marketID]
	if room == nil {
		room = make(map[*connection]struct{})
		h.marketRooms[marketID] = room
	}
	room[client] = struct{}{}
}

func (h *Hub) registerUser(client *connection, wallet string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	room := h.userRooms[wallet]
	if room == nil {
		room = make(map[*connection]struct{})
		h.userRooms[wallet] = room
	}
	room[client] = struct{}{}
	client.wallet = wallet
}

func (h *Hub) unregister(client *connection) {
	var (
		marketID     uint64
		marketConns  int
		userConns    int
		hasMarketWS  bool
		hasUserWS    bool
	)
	h.mu.Lock()
	if client.marketID != nil {
		hasMarketWS = true
		marketID = *client.marketID
		if room := h.marketRooms[*client.marketID]; room != nil {
			delete(room, client)
			marketConns = len(room)
			if len(room) == 0 {
				delete(h.marketRooms, *client.marketID)
				marketConns = 0
			}
		}
	}
	if client.wallet != "" {
		hasUserWS = true
		if room := h.userRooms[client.wallet]; room != nil {
			delete(room, client)
			userConns = len(room)
			if len(room) == 0 {
				delete(h.userRooms, client.wallet)
				userConns = 0
			}
		}
	}
	close(client.send)
	h.mu.Unlock()
	if hasMarketWS {
		logger.Infof("market ws disconnected market=%d remote=%s conns=%d", marketID, client.remote, marketConns)
	}
	if hasUserWS {
		logger.Infof("user ws disconnected wallet=%s remote=%s conns=%d", client.wallet, client.remote, userConns)
	}
}

func (h *Hub) marketStateFor(marketID uint64) *marketState {
	h.mu.Lock()
	defer h.mu.Unlock()
	state := h.marketState[marketID]
	if state == nil {
		state = &marketState{}
		h.marketState[marketID] = state
	}
	return state
}

func (h *Hub) buildMarketSnapshot(ctx context.Context, marketID uint64) ([]byte, uint64, protocol.WSMarketSnapshotPayload, error) {
	state := h.marketStateFor(marketID)
	state.ensureLoaded(ctx, marketID, h.marketData)
	seq, payload := state.snapshotPayload()
	body, err := buildSnapshotMessage(marketID, seq, payload)
	if err != nil {
		return nil, 0, protocol.WSMarketSnapshotPayload{}, err
	}
	return body, seq, payload, nil
}

func (h *Hub) broadcastMarket(marketID uint64, seq uint64, payload []byte) {
	h.mu.RLock()
	room := h.marketRooms[marketID]
	conns := make([]*connection, 0, len(room))
	for conn := range room {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()
	if len(conns) == 0 {
		logger.Infof("market ws broadcast skipped market=%d seq=%d conns=0", marketID, seq)
		return
	}
	logger.Infof("market ws broadcast market=%d seq=%d conns=%d bytes=%d", marketID, seq, len(conns), len(payload))
	for _, conn := range conns {
		conn.enqueueMarket(seq, payload)
	}
}

func (h *Hub) broadcastUser(wallet string, payload []byte) {
	h.mu.RLock()
	room := h.userRooms[wallet]
	conns := make([]*connection, 0, len(room))
	for conn := range room {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()
	for _, conn := range conns {
		conn.enqueue(payload)
	}
}

func (c *connection) enqueue(payload []byte) {
	c.enqueueRaw(payload)
}

func (c *connection) enqueueRaw(payload []byte) {
	select {
	case c.send <- payload:
	default:
		if c.marketID != nil {
			logger.Warnf("market ws send queue full market=%d remote=%s queue=%d", *c.marketID, c.remote, len(c.send))
		} else if c.wallet != "" {
			logger.Warnf("user ws send queue full wallet=%s remote=%s queue=%d", c.wallet, c.remote, len(c.send))
		}
		_ = c.ws.Close()
	}
}

func (c *connection) enqueueMarket(seq uint64, payload []byte) {
	c.bootMu.Lock()
	if c.bootstrapping {
		c.pending = append(c.pending, queuedMarketMessage{
			seq:     seq,
			payload: append([]byte(nil), payload...),
		})
		c.bootMu.Unlock()
		return
	}
	c.bootMu.Unlock()
	c.enqueueRaw(payload)
}

func (c *connection) finishMarketBootstrap(snapshotSeq uint64, snapshotPayload []byte) {
	c.enqueueRaw(snapshotPayload)
	for {
		c.bootMu.Lock()
		pending := make([]queuedMarketMessage, 0, len(c.pending))
		for _, item := range c.pending {
			if item.seq > snapshotSeq {
				pending = append(pending, item)
			}
		}
		c.pending = c.pending[:0]
		if len(pending) == 0 {
			c.bootstrapping = false
			c.bootMu.Unlock()
			return
		}
		c.bootMu.Unlock()
		for _, item := range pending {
			c.enqueueRaw(item.payload)
		}
	}
}

func (c *connection) readPump() {
	defer func() {
		c.hub.unregister(c)
		_ = c.ws.Close()
	}()

	_ = c.ws.SetReadDeadline(time.Now().Add(pongTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		if _, _, err := c.ws.ReadMessage(); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, websocket.ErrCloseSent) {
				return
			}
			if c.marketID != nil {
				logger.Warnf("market ws read failed market=%d remote=%s err=%v", *c.marketID, c.remote, err)
			} else if c.wallet != "" {
				logger.Warnf("user ws read failed wallet=%s remote=%s err=%v", c.wallet, c.remote, err)
			}
			return
		}
	}
}

func (h *Hub) consumeTicket(ctx context.Context, ticket string) (string, error) {
	if h.tickets == nil || !h.tickets.Enabled() {
		return "", errors.New("websocket ticket store is not configured")
	}
	wallet, err := h.tickets.Consume(ctx, ticket)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(wallet) == "" {
		return "", errors.New("invalid websocket ticket")
	}
	return wallet, nil
}

func (c *connection) writePump() {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case payload, ok := <-c.send:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if !ok {
				_ = c.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
				return
			}
			if err := c.ws.WriteMessage(websocket.TextMessage, payload); err != nil {
				if c.marketID != nil {
					logger.Warnf("market ws write failed market=%d remote=%s err=%v", *c.marketID, c.remote, err)
				} else if c.wallet != "" {
					logger.Warnf("user ws write failed wallet=%s remote=%s err=%v", c.wallet, c.remote, err)
				}
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				if c.marketID != nil {
					logger.Warnf("market ws ping failed market=%d remote=%s err=%v", *c.marketID, c.remote, err)
				} else if c.wallet != "" {
					logger.Warnf("user ws ping failed wallet=%s remote=%s err=%v", c.wallet, c.remote, err)
				}
				return
			}
		}
	}
}

func (h *Hub) marketConnectionCount(marketID uint64) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.marketRooms[marketID])
}

func (h *Hub) userConnectionCount(wallet string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.userRooms[wallet])
}
