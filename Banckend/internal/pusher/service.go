package pusher

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"

	"github.com/nats-io/nats.go"
)

var logger = logging.New("pusher")

const hotConsumerSubject = protocol.SubjectMarketDeltaHot + ".*"

type Service struct {
	client *natsjs.Client
	hub    *Hub
	sub    *nats.Subscription
}

func NewService(client *natsjs.Client, hub *Hub) *Service {
	return &Service{
		client: client,
		hub:    hub,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s.client == nil || s.hub == nil {
		return nil
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	go func() {
		<-ctx.Done()
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	}()
	return nil
}

func (s *Service) ensureSubscription() error {
	if s.sub != nil {
		return nil
	}
	conn := s.client.Conn()
	if conn == nil {
		return fmt.Errorf("nats core connection is not configured")
	}
	sub, err := conn.Subscribe(hotConsumerSubject, s.handleMessage)
	if err != nil {
		return fmt.Errorf("subscribe hot market stream: %w", err)
	}
	s.sub = sub
	logger.Infof("pusher subscribed subject=%s", hotConsumerSubject)
	return nil
}

func (s *Service) handleMessage(msg *nats.Msg) {
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		logger.Warnf("decode hot market delta failed subject=%s err=%v", msg.Subject, err)
		return
	}
	if event.SchemaVersion != 0 && event.SchemaVersion != 1 {
		logger.Warnf("unsupported hot market delta schema subject=%s version=%d", msg.Subject, event.SchemaVersion)
		return
	}
	if err := s.hub.PublishMarketDelta(context.Background(), event); err != nil {
		logger.Warnf("publish hot market delta failed market=%d subject=%s err=%v", event.MarketID, msg.Subject, err)
		return
	}
	logger.Infof(
		"hot market delta published market=%d event=%s source_seq=%d seq_range=%d-%d depths=%d fills=%d order_updates=%d conns=%d",
		event.MarketID,
		event.EventID,
		event.SourceCmdSeqMax,
		event.SourceCmdSeqMin,
		event.SourceCmdSeqMax,
		len(event.DepthUpdates),
		len(event.Fills),
		len(event.OrderUpdates),
		s.hub.marketConnectionCount(event.MarketID),
	)
}

func compressDepthUpdates(updates []matching.DepthUpdate) []protocol.WSDepthLevel {
	type key struct {
		side      string
		priceTick uint8
	}
	latest := make(map[key]matching.DepthUpdate, len(updates))
	order := make([]key, 0, len(updates))
	for _, update := range updates {
		k := key{side: update.Side, priceTick: update.PriceTick}
		if _, ok := latest[k]; !ok {
			order = append(order, k)
		}
		latest[k] = update
	}

	levels := make([]protocol.WSDepthLevel, 0, len(order))
	for _, k := range order {
		update := latest[k]
		levels = append(levels, protocol.WSDepthLevel{
			Side:        update.Side,
			PriceTick:   update.PriceTick,
			TotalVolume: update.TotalVolume,
		})
	}
	return levels
}

func tradeIDForFill(eventID string, fill matching.MatchFill) string {
	if eventID == "" {
		return "fill-" + strconv.FormatUint(uint64(fill.FillIndex), 10)
	}
	return eventID + "-" + strconv.FormatUint(uint64(fill.FillIndex), 10)
}
