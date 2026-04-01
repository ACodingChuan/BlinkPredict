package marketindexer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/markets"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/rs/zerolog"
)

const SubjectMarketCreated = "webhook.market.created"

type MarketCreatedPayload struct {
	MarketID            string `json:"market_id"`
	MarketPDA           string `json:"market_pda"`
	Creator             string `json:"creator"`
	MetadataCID         string `json:"metadata_cid"`
	MetadataURL         string `json:"metadata_url,omitempty"`
	ResolutionMode      string `json:"resolution_mode"`
	ResolutionAuthority string `json:"resolution_authority,omitempty"`
	OracleFeed          string `json:"oracle_feed,omitempty"`
	OracleCondition     string `json:"oracle_condition,omitempty"`
	OracleTargetPrice   int64  `json:"oracle_target_price,omitempty"`
	OracleTargetExpo    int32  `json:"oracle_target_expo,omitempty"`
	CloseTS             int64  `json:"close_ts"`
	ResolveAfterTS      int64  `json:"resolve_after_ts"`
	ClaimDeadlineTS     int64  `json:"claim_deadline_ts"`
	Signature           string `json:"signature,omitempty"`
}

type Consumer struct {
	nc          *nats.Conn
	pool        *pgxpool.Pool
	marketRepo  markets.Repository
	marketCache *cache.MarketCache
	log         *zerolog.Logger
	sub         *nats.Subscription
}

func NewConsumer(nc *nats.Conn, pool *pgxpool.Pool, marketRepo markets.Repository, marketCache *cache.MarketCache, log *zerolog.Logger) *Consumer {
	return &Consumer{nc: nc, pool: pool, marketRepo: marketRepo, marketCache: marketCache, log: log}
}

func (c *Consumer) Start(ctx context.Context) error {
	if c == nil || c.nc == nil || c.marketRepo == nil {
		return nil
	}
	sub, err := c.nc.Subscribe(SubjectMarketCreated, c.handleMarketCreated)
	if err != nil {
		return fmt.Errorf("subscribe %s: %w", SubjectMarketCreated, err)
	}
	c.sub = sub
	go func() {
		<-ctx.Done()
		if c.sub != nil {
			_ = c.sub.Unsubscribe()
		}
	}()
	return nil
}

func (c *Consumer) handleMarketCreated(msg *nats.Msg) {
	var payload MarketCreatedPayload
	if err := json.Unmarshal(msg.Data, &payload); err != nil {
		if c.log != nil {
			c.log.Error().Err(err).Msg("decode market created payload failed")
		}
		return
	}

	metadataURL := payload.MetadataURL
	if metadataURL == "" && strings.TrimSpace(payload.MetadataCID) != "" {
		metadataURL = "ipfs://" + strings.TrimSpace(payload.MetadataCID)
	}
	meta, err := fetchMetadata(metadataURL)
	if err != nil && c.log != nil {
		c.log.Warn().Err(err).Str("metadata_url", metadataURL).Msg("fetch metadata failed; using payload fallbacks")
	}

	marketID, err := parseMarketID(payload.MarketID)
	if err != nil {
		if c.log != nil {
			c.log.Error().Err(err).Str("market_id", payload.MarketID).Msg("invalid market_id in market created payload")
		}
		return
	}

	now := time.Now().UTC()
	market := markets.Market{
		ID:          uuid.NewString(),
		MarketID:    marketID,
		MarketPDA:   payload.MarketPDA,
		MetadataCID: payload.MetadataCID,
		MetadataURL: metadataURL,
		Title:       meta.Title,
		Description: meta.Description,
		ImageURL:    meta.ImageURL,
		Status:      markets.MarketStatusOpen,
		Outcome:     markets.MarketOutcomeUndecided,
		Resolution: markets.ResolutionConfig{
			Mode:             markets.ResolutionMode(payload.ResolutionMode),
			Authority:        payload.ResolutionAuthority,
			OracleFeed:       payload.OracleFeed,
			OracleCondition:  markets.OracleCondition(payload.OracleCondition),
			OracleTarget:     uint64(max64(payload.OracleTargetPrice)),
			OracleTargetExpo: payload.OracleTargetExpo,
		},
		CloseTime:         time.Unix(payload.CloseTS, 0),
		ResolveAfterTime:  time.Unix(payload.ResolveAfterTS, 0),
		ClaimDeadlineTime: time.Unix(payload.ClaimDeadlineTS, 0),
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	if meta.Title == "" {
		market.Title = fmt.Sprintf("Market %d", marketID)
	}
	if err := c.marketRepo.Save(context.Background(), market); err != nil {
		if c.log != nil {
			c.log.Error().Err(err).Uint64("market_id", marketID).Msg("save indexed market failed")
		}
		return
	}
	if c.marketCache != nil {
		_ = c.marketCache.SetMarket(context.Background(), cache.MarketData{
			ID:                   market.ID,
			MarketID:             market.MarketID,
			MarketPDA:            market.MarketPDA,
			MetadataCID:          market.MetadataCID,
			MetadataURL:          market.MetadataURL,
			Title:                market.Title,
			Description:          market.Description,
			ImageURL:             market.ImageURL,
			Status:               string(market.Status),
			Outcome:              string(market.Outcome),
			ResolutionMode:       string(market.Resolution.Mode),
			ResolutionAuthority:  market.Resolution.Authority,
			OracleFeed:           market.Resolution.OracleFeed,
			OracleCondition:      string(market.Resolution.OracleCondition),
			OracleTargetPrice:    int64(market.Resolution.OracleTarget),
			CloseTime:            market.CloseTime.Unix(),
			ResolveAfterTime:     market.ResolveAfterTime.Unix(),
			ClaimDeadlineTime:    market.ClaimDeadlineTime.Unix(),
			CreatorUnclaimedFee:  market.CreatorUnclaimedFee,
			PlatformUnclaimedFee: market.PlatformUnclaimedFee,
			CreatedAt:            market.CreatedAt.Unix(),
			UpdatedAt:            market.UpdatedAt.Unix(),
		})
	}
}

type metadataDocument struct {
	Title       string `json:"title"`
	Description string `json:"description"`
	Image       string `json:"image"`
	ImageURL    string `json:"image_url"`
}

func fetchMetadata(uri string) (*metadataDocument, error) {
	if strings.TrimSpace(uri) == "" {
		return &metadataDocument{}, nil
	}
	url := uri
	if strings.HasPrefix(url, "ipfs://") {
		url = "https://ipfs.io/ipfs/" + strings.TrimPrefix(url, "ipfs://")
	}
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("metadata http status=%d", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var doc metadataDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, err
	}
	if doc.ImageURL == "" {
		doc.ImageURL = doc.Image
	}
	return &doc, nil
}

func parseMarketID(raw string) (uint64, error) {
	return strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
}

func max64(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}
