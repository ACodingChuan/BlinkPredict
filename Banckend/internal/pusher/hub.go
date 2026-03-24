package pusher

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"time"

	"blinkpredict/banckend/internal/auth"
	"blinkpredict/banckend/internal/config"
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
	cfg config.Config

	upgrader websocket.Upgrader

	mu          sync.RWMutex
	marketRooms map[uint64]map[*connection]struct{}
	userRooms   map[string]map[*connection]struct{}
}

type connection struct {
	hub      *Hub
	ws       *websocket.Conn
	send     chan []byte
	marketID *uint64
	wallet   string
	authed   bool
}

type userAuthFrame struct {
	PrivyToken string `json:"privy_token"`
}

func NewHub(cfg config.Config) *Hub {
	return &Hub{
		cfg: cfg,
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
		hub:      h,
		ws:       ws,
		send:     make(chan []byte, sendQueueSize),
		marketID: &marketID,
	}
	h.registerMarket(client, marketID)
	go client.writePump()
	client.readPump(false)
	return nil
}

func (h *Hub) ServeUserWS(w http.ResponseWriter, r *http.Request) error {
	ws, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return err
	}
	client := &connection{
		hub:  h,
		ws:   ws,
		send: make(chan []byte, sendQueueSize),
	}
	go client.writePump()
	client.readPump(true)
	return nil
}

func (h *Hub) PublishMarketDepth(payload protocol.MarketDepthPush) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	marketID, err := strconv.ParseUint(payload.MarketID, 10, 64)
	if err != nil {
		return err
	}
	h.broadcastMarket(marketID, body)
	return nil
}

func (h *Hub) PublishMarketTrade(payload protocol.MarketTradePush) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	marketID, err := strconv.ParseUint(payload.MarketID, 10, 64)
	if err != nil {
		return err
	}
	h.broadcastMarket(marketID, body)
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
	client.authed = true
}

func (h *Hub) unregister(client *connection) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if client.marketID != nil {
		if room := h.marketRooms[*client.marketID]; room != nil {
			delete(room, client)
			if len(room) == 0 {
				delete(h.marketRooms, *client.marketID)
			}
		}
	}
	if client.wallet != "" {
		if room := h.userRooms[client.wallet]; room != nil {
			delete(room, client)
			if len(room) == 0 {
				delete(h.userRooms, client.wallet)
			}
		}
	}
	close(client.send)
}

func (h *Hub) broadcastMarket(marketID uint64, payload []byte) {
	h.mu.RLock()
	room := h.marketRooms[marketID]
	conns := make([]*connection, 0, len(room))
	for conn := range room {
		conns = append(conns, conn)
	}
	h.mu.RUnlock()
	for _, conn := range conns {
		conn.enqueue(payload)
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
	select {
	case c.send <- payload:
	default:
		_ = c.ws.Close()
	}
}

func (c *connection) readPump(requiresAuth bool) {
	defer func() {
		c.hub.unregister(c)
		_ = c.ws.Close()
	}()

	if requiresAuth {
		if err := c.authenticate(); err != nil {
			_ = c.ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.ClosePolicyViolation, err.Error()))
			return
		}
	}

	_ = c.ws.SetReadDeadline(time.Now().Add(pongTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(pongTimeout))
	})

	for {
		if _, _, err := c.ws.ReadMessage(); err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) || errors.Is(err, websocket.ErrCloseSent) {
				return
			}
			return
		}
	}
}

func (c *connection) authenticate() error {
	_ = c.ws.SetReadDeadline(time.Now().Add(authTimeout))
	_, payload, err := c.ws.ReadMessage()
	if err != nil {
		return err
	}
	var frame userAuthFrame
	if err := json.Unmarshal(payload, &frame); err != nil {
		return err
	}
	if frame.PrivyToken == "" {
		return errors.New("missing privy token")
	}
	user, err := auth.ParseToken(frame.PrivyToken, c.hub.cfg)
	if err != nil {
		return err
	}
	if user.SolanaAddress == "" {
		return errors.New("missing solana wallet")
	}
	c.hub.registerUser(c, user.SolanaAddress)
	return nil
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
				return
			}
		case <-ticker.C:
			_ = c.ws.SetWriteDeadline(time.Now().Add(writeTimeout))
			if err := c.ws.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
