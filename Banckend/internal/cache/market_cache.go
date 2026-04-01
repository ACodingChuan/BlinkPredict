package cache

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

const (
	// Redis keys
	KeyMarketDetail    = "market:%d"        // market:123
	KeyMarketsAll      = "markets:all"      // 所有市场
	KeyMarketsOpen     = "markets:open"     // 开放中市场
	KeyMarketsResolved = "markets:resolved" // 已解决市场

	// TTL 常量
	TTLMarketDetail = 24 * time.Hour // 市场详情缓存 24 小时
	TTLMarketList   = 24 * time.Hour // 市场列表缓存 24 小时
)

// MarketCache Redis 缓存层
type MarketCache struct {
	redis *redis.Client
	log   *zerolog.Logger
}

// NewMarketCache 创建市场缓存实例
func NewMarketCache(redisClient *redis.Client, logger *zerolog.Logger) *MarketCache {
	return &MarketCache{
		redis: redisClient,
		log:   logger,
	}
}

// MarketData Redis 中存储的市场数据结构
type MarketData struct {
	ID                   string `json:"id"`
	MarketID             uint64 `json:"market_id"`
	MarketPDA            string `json:"market_pda"`
	MetadataCID          string `json:"metadata_cid"`
	MetadataURL          string `json:"metadata_url"`
	CollateralMint       string `json:"collateral_mint"`
	Title                string `json:"title"`
	Description          string `json:"description"`
	Category             string `json:"category"`
	ImageURL             string `json:"image_url"`
	Status               string `json:"status"`
	Outcome              string `json:"outcome"`
	ResolutionMode       string `json:"resolution_mode"`
	ResolutionAuthority  string `json:"resolution_authority"`
	OracleFeed           string `json:"oracle_feed"`
	OracleCondition      string `json:"oracle_condition"`
	OracleTargetPrice    int64  `json:"oracle_target_price"`
	OracleTargetExpo     int32  `json:"oracle_target_expo"`
	CloseTime            int64  `json:"close_time"`
	ResolveAfterTime     int64  `json:"resolve_after_time"`
	ClaimDeadlineTime    int64  `json:"claim_deadline_time"`
	CreatorUnclaimedFee  int64  `json:"creator_unclaimed_fee"`
	PlatformUnclaimedFee int64  `json:"platform_unclaimed_fee"`
	CreatedAt            int64  `json:"created_at"`
	UpdatedAt            int64  `json:"updated_at"`
}

// SetMarket 设置市场详情缓存
func (c *MarketCache) SetMarket(ctx context.Context, market MarketData) error {
	key := fmt.Sprintf(KeyMarketDetail, market.MarketID)

	// 记录日志
	c.log.Info().
		Uint64("market_id", market.MarketID).
		Str("key", key).
		Msg("Setting market detail in Redis")

	// 将数据转换为 map
	data := map[string]interface{}{
		"id":                     market.ID,
		"market_id":              market.MarketID,
		"market_pda":             market.MarketPDA,
		"metadata_cid":           market.MetadataCID,
		"metadata_url":           market.MetadataURL,
		"collateral_mint":        market.CollateralMint,
		"title":                  market.Title,
		"description":            market.Description,
		"category":               market.Category,
		"image_url":              market.ImageURL,
		"status":                 market.Status,
		"outcome":                market.Outcome,
		"resolution_mode":        market.ResolutionMode,
		"resolution_authority":   market.ResolutionAuthority,
		"oracle_feed":            market.OracleFeed,
		"oracle_condition":       market.OracleCondition,
		"oracle_target_price":    market.OracleTargetPrice,
		"oracle_target_expo":     market.OracleTargetExpo,
		"close_time":             market.CloseTime,
		"resolve_after_time":     market.ResolveAfterTime,
		"claim_deadline_time":    market.ClaimDeadlineTime,
		"creator_unclaimed_fee":  market.CreatorUnclaimedFee,
		"platform_unclaimed_fee": market.PlatformUnclaimedFee,
		"created_at":             market.CreatedAt,
		"updated_at":             market.UpdatedAt,
	}

	// 使用 HMSET 存储所有字段
	if err := c.redis.HMSet(ctx, key, data).Err(); err != nil {
		c.log.Error().
			Err(err).
			Str("key", key).
			Msg("Failed to set market detail in Redis")
		return fmt.Errorf("failed to HMSET: %w", err)
	}

	// 设置过期时间
	if err := c.redis.Expire(ctx, key, TTLMarketDetail).Err(); err != nil {
		c.log.Error().
			Err(err).
			Str("key", key).
			Dur("ttl", TTLMarketDetail).
			Msg("Failed to set TTL for market detail")
		return fmt.Errorf("failed to set TTL: %w", err)
	}

	// 同时添加到列表缓存
	if err := c.AddMarketToList(ctx, market); err != nil {
		c.log.Error().
			Err(err).
			Uint64("market_id", market.MarketID).
			Msg("Failed to add market to list cache")
		// 这里不返回错误，因为详情已经设置成功
	}

	c.log.Info().
		Uint64("market_id", market.MarketID).
		Str("key", key).
		Dur("ttl", TTLMarketDetail).
		Msg("Market detail set successfully in Redis")

	return nil
}

// GetMarket 获取市场详情缓存
func (c *MarketCache) GetMarket(ctx context.Context, marketID uint64) (*MarketData, error) {
	key := fmt.Sprintf(KeyMarketDetail, marketID)

	c.log.Debug().
		Uint64("market_id", marketID).
		Str("key", key).
		Msg("Getting market detail from Redis")

	// HGETALL 获取所有字段
	data, err := c.redis.HGetAll(ctx, key).Result()
	if err != nil {
		if err == redis.Nil {
			c.log.Debug().
				Uint64("market_id", marketID).
				Msg("Market not found in Redis (cache miss)")
			return nil, fmt.Errorf("market %d not found", marketID)
		}
		c.log.Error().
			Err(err).
			Str("key", key).
			Msg("Failed to get market from Redis")
		return nil, fmt.Errorf("failed to HGETALL: %w", err)
	}

	if len(data) == 0 {
		c.log.Debug().
			Uint64("market_id", marketID).
			Msg("Market data is empty in Redis")
		return nil, fmt.Errorf("market %d not found", marketID)
	}

	// 解析数据
	market, err := c.parseMarketData(data)
	if err != nil {
		c.log.Error().
			Err(err).
			Uint64("market_id", marketID).
			Msg("Failed to parse market data from Redis")
		return nil, err
	}

	c.log.Debug().
		Uint64("market_id", marketID).
		Str("title", market.Title).
		Msg("Market retrieved from Redis successfully")

	return market, nil
}

// AddMarketToList 添加市场到列表缓存
func (c *MarketCache) AddMarketToList(ctx context.Context, market MarketData) error {
	pipe := c.redis.Pipeline()

	// 1. 添加到全部市场（按创建时间排序）
	pipe.ZAdd(ctx, KeyMarketsAll, redis.Z{
		Score:  float64(market.CreatedAt),
		Member: market.MarketID,
	})

	// 2. 添加到对应状态集合
	if market.Status == "open" {
		pipe.ZAdd(ctx, KeyMarketsOpen, redis.Z{
			Score:  float64(market.CreatedAt),
			Member: market.MarketID,
		})
	} else if market.Status == "resolved" {
		pipe.ZAdd(ctx, KeyMarketsResolved, redis.Z{
			Score:  float64(market.CreatedAt),
			Member: market.MarketID,
		})
	}

	// 3. 设置列表缓存过期时间
	pipe.Expire(ctx, KeyMarketsAll, TTLMarketList)
	pipe.Expire(ctx, KeyMarketsOpen, TTLMarketList)
	pipe.Expire(ctx, KeyMarketsResolved, TTLMarketList)

	// 执行 pipeline
	_, err := pipe.Exec(ctx)
	if err != nil {
		c.log.Error().
			Err(err).
			Uint64("market_id", market.MarketID).
			Msg("Failed to execute pipeline for market lists")
		return fmt.Errorf("failed to execute pipeline: %w", err)
	}

	c.log.Info().
		Uint64("market_id", market.MarketID).
		Str("status", market.Status).
		Msg("Market added to list cache successfully")

	return nil
}

// GetMarketsByStatus 根据状态获取市场列表（分页）
func (c *MarketCache) GetMarketsByStatus(ctx context.Context, status string, page, pageSize int) ([]uint64, error) {
	var key string
	switch status {
	case "all":
		key = KeyMarketsAll
	case "open":
		key = KeyMarketsOpen
	case "resolved":
		key = KeyMarketsResolved
	default:
		return nil, fmt.Errorf("invalid status: %s", status)
	}

	c.log.Debug().
		Str("status", status).
		Int("page", page).
		Int("page_size", pageSize).
		Str("redis_key", key).
		Msg("Getting markets by status from Redis")

	// 计算分页范围
	start := int64((page - 1) * pageSize)
	end := start + int64(pageSize) - 1

	// 使用 ZREVRANGE 按时间倒序获取
	marketIDs, err := c.redis.ZRevRange(ctx, key, start, end).Result()
	if err != nil {
		if err == redis.Nil {
			c.log.Debug().
				Str("status", status).
				Msg("No markets found in Redis for this status")
			return []uint64{}, nil
		}
		c.log.Error().
			Err(err).
			Str("key", key).
			Msg("Failed to get market list from Redis")
		return nil, fmt.Errorf("failed to ZREVRANGE: %w", err)
	}

	// 转换字符串为 uint64
	ids := make([]uint64, 0, len(marketIDs))
	for _, idStr := range marketIDs {
		var id uint64
		if _, err := fmt.Sscanf(idStr, "%d", &id); err == nil {
			ids = append(ids, id)
		}
	}

	c.log.Debug().
		Str("status", status).
		Int("count", len(ids)).
		Msg("Markets retrieved from Redis successfully")

	return ids, nil
}

// DeleteMarket 删除市场缓存
func (c *MarketCache) DeleteMarket(ctx context.Context, marketID uint64) error {
	key := fmt.Sprintf(KeyMarketDetail, marketID)

	c.log.Info().
		Uint64("market_id", marketID).
		Str("key", key).
		Msg("Deleting market from Redis")

	// 删除详情缓存
	if err := c.redis.Del(ctx, key).Err(); err != nil {
		c.log.Error().
			Err(err).
			Uint64("market_id", marketID).
			Msg("Failed to delete market detail from Redis")
		return fmt.Errorf("failed to delete market detail: %w", err)
	}

	// 从列表中移除
	pipe := c.redis.Pipeline()
	pipe.ZRem(ctx, KeyMarketsAll, marketID)
	pipe.ZRem(ctx, KeyMarketsOpen, marketID)
	pipe.ZRem(ctx, KeyMarketsResolved, marketID)

	if _, err := pipe.Exec(ctx); err != nil {
		c.log.Error().
			Err(err).
			Uint64("market_id", marketID).
			Msg("Failed to remove market from lists")
		// 不返回错误，因为详情已经删除
	}

	c.log.Info().
		Uint64("market_id", marketID).
		Msg("Market deleted from Redis successfully")

	return nil
}

// parseMarketData 从 Redis hash 解析市场数据
func (c *MarketCache) parseMarketData(data map[string]string) (*MarketData, error) {
	market := &MarketData{}

	// 必需字段解析
	if id, ok := data["id"]; ok {
		market.ID = id
	}
	if marketIDStr, ok := data["market_id"]; ok {
		if parsed, err := strconv.ParseUint(marketIDStr, 10, 64); err == nil {
			market.MarketID = parsed
		}
	}

	// 可选字段解析
	market.MarketPDA = data["market_pda"]
	market.MetadataCID = data["metadata_cid"]
	market.MetadataURL = data["metadata_url"]
	market.CollateralMint = data["collateral_mint"]
	market.Title = data["title"]
	market.Description = data["description"]
	market.Category = data["category"]
	market.ImageURL = data["image_url"]
	market.Status = data["status"]
	market.Outcome = data["outcome"]
	market.ResolutionMode = data["resolution_mode"]
	market.ResolutionAuthority = data["resolution_authority"]
	market.OracleFeed = data["oracle_feed"]
	market.OracleCondition = data["oracle_condition"]

	// 数字字段解析
	if closeTime, ok := data["close_time"]; ok {
		fmt.Sscanf(closeTime, "%d", &market.CloseTime)
	}
	if resolveAfterTime, ok := data["resolve_after_time"]; ok {
		fmt.Sscanf(resolveAfterTime, "%d", &market.ResolveAfterTime)
	}
	if claimDeadlineTime, ok := data["claim_deadline_time"]; ok {
		fmt.Sscanf(claimDeadlineTime, "%d", &market.ClaimDeadlineTime)
	}
	if targetPrice, ok := data["oracle_target_price"]; ok {
		fmt.Sscanf(targetPrice, "%d", &market.OracleTargetPrice)
	}
	if targetExpo, ok := data["oracle_target_expo"]; ok {
		fmt.Sscanf(targetExpo, "%d", &market.OracleTargetExpo)
	}
	if creatorUnclaimedFee, ok := data["creator_unclaimed_fee"]; ok {
		fmt.Sscanf(creatorUnclaimedFee, "%d", &market.CreatorUnclaimedFee)
	}
	if platformUnclaimedFee, ok := data["platform_unclaimed_fee"]; ok {
		fmt.Sscanf(platformUnclaimedFee, "%d", &market.PlatformUnclaimedFee)
	}
	if createdAt, ok := data["created_at"]; ok {
		fmt.Sscanf(createdAt, "%d", &market.CreatedAt)
	}
	if updatedAt, ok := data["updated_at"]; ok {
		fmt.Sscanf(updatedAt, "%d", &market.UpdatedAt)
	}

	return market, nil
}

// LogMarketData 详细记录市场数据（调试用）
func (c *MarketCache) LogMarketData(market MarketData) {
	jsonData, _ := json.MarshalIndent(market, "", "  ")
	c.log.Debug().
		RawJSON("market_data", jsonData).
		Msg("Market data details")
}
