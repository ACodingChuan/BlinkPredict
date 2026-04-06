package marketconfirm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/markets"
	"blinkpredict/banckend/internal/protocol"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var projectorLogger = logging.New("marketprojector")

const (
	marketProjectorFetchBatch = 16
	marketProjectorMaxWait    = 1500 * time.Millisecond
)

type Projector struct {
	pool        *pgxpool.Pool
	client      *natsjs.Client
	marketRepo  markets.Repository
	marketCache *cache.MarketCache
	sub         *nats.Subscription
}

func NewProjector(client *natsjs.Client, pool *pgxpool.Pool, marketRepo markets.Repository, marketCache *cache.MarketCache) *Projector {
	return &Projector{pool: pool, client: client, marketRepo: marketRepo, marketCache: marketCache}
}

func (p *Projector) Start(ctx context.Context) error {
	if p == nil || p.client == nil || p.marketRepo == nil {
		return nil
	}
	if err := p.ensureSubscription(); err != nil {
		return err
	}
	go p.run(ctx)
	return nil
}

func (p *Projector) ensureSubscription() error {
	if p.sub != nil {
		return nil
	}
	sub, err := p.client.PullSubscribe(protocol.SubjectMarketConfirmed, "market-projector")
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", protocol.SubjectMarketConfirmed, err)
	}
	p.sub = sub
	return nil
}

func (p *Projector) run(ctx context.Context) {
	defer func() {
		if p.sub != nil {
			_ = p.sub.Unsubscribe()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgs, err := p.sub.Fetch(marketProjectorFetchBatch, nats.MaxWait(marketProjectorMaxWait))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			projectorLogger.Warnf("market projector fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			p.handleMessage(msg)
		}
	}
}

func (p *Projector) handleMessage(msg *nats.Msg) {
	var event protocol.MarketConfirmedEvent
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		projectorLogger.Warnf("market projector decode failed: %v", err)
		_ = msg.Term()
		return
	}
	now := time.Now().UTC()
	market := markets.Market{
		ID:             uuid.NewString(),
		MarketID:       event.MarketID,
		MarketPDA:      event.MarketPDA,
		MetadataCID:    event.MetadataCID,
		MetadataURL:    event.MetadataURL,
		CollateralMint: "",
		Title:          fallbackTitle(event.Title, event.MarketID),
		Description:    event.Description,
		Category:       event.Category,
		ImageURL:       event.ImageURL,
		Status:         markets.MarketStatusOpen,
		Outcome:        markets.MarketOutcomeUndecided,
		Resolution: markets.ResolutionConfig{
			Mode:             markets.ResolutionMode(event.ResolutionMode),
			Authority:        event.ResolutionAuthority,
			OracleFeed:       event.OracleFeed,
			OracleCondition:  markets.OracleCondition(event.OracleCondition),
			OracleTarget:     event.OracleTargetPrice,
			OracleTargetExpo: event.OracleTargetExpo,
		},
		CloseTime:         time.Unix(event.CloseTS, 0).UTC(),
		ResolveAfterTime:  time.Unix(event.ResolveAfterTS, 0).UTC(),
		ClaimDeadlineTime: time.Unix(event.ClaimDeadlineTS, 0).UTC(),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	projectorLogger.Infof("market projector saving signature=%s market_id=%d market_pda=%s", event.Signature, event.MarketID, event.MarketPDA)
	if err := p.marketRepo.Save(context.Background(), market); err != nil {
		projectorLogger.Warnf("market projector save failed signature=%s market_id=%d err=%v", event.Signature, event.MarketID, err)
		_ = msg.Nak()
		return
	}
	if p.pool != nil {
		if _, err := p.pool.Exec(context.Background(), `
			UPDATE market_submissions
			SET status = 'confirmed', market_id = $2, market_pda = $3, creator_wallet = $4, metadata_cid = $5, slot = $6, failure_reason = '', confirmed_at = COALESCE(confirmed_at, NOW()), updated_at = NOW()
			WHERE signature = $1
		`, event.Signature, fmt.Sprintf("%d", event.MarketID), event.MarketPDA, event.Creator, event.MetadataCID, int64(event.Slot)); err != nil {
			projectorLogger.Warnf("market projector update submission failed signature=%s err=%v", event.Signature, err)
			_ = msg.Nak()
			return
		}
	}
	if p.marketCache != nil {
		if err := p.marketCache.SetMarket(context.Background(), cache.MarketData{
			ID:                   market.ID,
			MarketID:             market.MarketID,
			MarketPDA:            market.MarketPDA,
			MetadataCID:          market.MetadataCID,
			MetadataURL:          market.MetadataURL,
			CollateralMint:       market.CollateralMint,
			Title:                market.Title,
			Description:          market.Description,
			Category:             market.Category,
			ImageURL:             market.ImageURL,
			Status:               string(market.Status),
			Outcome:              string(market.Outcome),
			ResolutionMode:       string(market.Resolution.Mode),
			ResolutionAuthority:  market.Resolution.Authority,
			OracleFeed:           market.Resolution.OracleFeed,
			OracleCondition:      string(market.Resolution.OracleCondition),
			OracleTargetPrice:    int64(market.Resolution.OracleTarget),
			OracleTargetExpo:     market.Resolution.OracleTargetExpo,
			CloseTime:            market.CloseTime.Unix(),
			ResolveAfterTime:     market.ResolveAfterTime.Unix(),
			ClaimDeadlineTime:    market.ClaimDeadlineTime.Unix(),
			CreatorUnclaimedFee:  market.CreatorUnclaimedFee,
			PlatformUnclaimedFee: market.PlatformUnclaimedFee,
			CreatedAt:            market.CreatedAt.Unix(),
			UpdatedAt:            market.UpdatedAt.Unix(),
		}); err != nil {
			projectorLogger.Warnf("market projector redis cache failed signature=%s market_id=%d err=%v", event.Signature, event.MarketID, err)
		}
	}
	projectorLogger.Infof("market projector saved signature=%s market_id=%d", event.Signature, event.MarketID)
	_ = msg.Ack()
}

func fallbackTitle(title string, marketID uint64) string {
	if title != "" {
		return title
	}
	return fmt.Sprintf("Market %d", marketID)
}
