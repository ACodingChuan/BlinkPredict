package webhooks

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"
	"github.com/valyala/fasthttp"

	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/markets"
)

// HeliusHandler Helius webhook 处理器
type HeliusHandler struct {
	markets     markets.Repository
	marketCache *cache.MarketCache
	log         *zerolog.Logger
	webhookSecret string
}

// NewHeliusHandler 创建 Helius webhook 处理器
func NewHeliusHandler(
	marketsRepo markets.Repository,
	marketCache *cache.MarketCache,
	logger *zerolog.Logger,
	webhookSecret string,
) *HeliusHandler {
	return &HeliusHandler{
		markets:       marketsRepo,
		marketCache:   marketCache,
		log:           logger,
		webhookSecret: webhookSecret,
	}
}

// HandleWebhook 处理 Helius webhook 请求
func (h *HeliusHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	requestID := uuid.New().String()

	// 🔍 日志 1: 记录请求基本信息
	h.log.Info().
		Str("request_id", requestID).
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Str("remote_addr", r.RemoteAddr).
		Msg("📥 Received Helius webhook request")

	// 1. 验证请求方法
	if r.Method != http.MethodPost {
		h.log.Warn().
			Str("request_id", requestID).
			Str("method", r.Method).
			Msg("❌ Invalid method, only POST allowed")
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 2. 验证 Auth Header（安全验证）
	authHeader := r.Header.Get("Authorization")

	// 支持两种格式：
	// 1. "Bearer <token>" (标准格式)
	// 2. "<token>" (Helius 直接发送 token)
	var receivedToken string
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		receivedToken = authHeader[7:]
	} else {
		receivedToken = authHeader
	}

	h.log.Debug().
		Str("request_id", requestID).
		Str("auth_header", authHeader).
		Str("expected_token", h.webhookSecret).
		Msg("🔐 Verifying webhook authentication")

	if receivedToken != h.webhookSecret {
		h.log.Error().
			Str("request_id", requestID).
			Str("received_token", receivedToken).
			Msg("❌ Invalid webhook authentication")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	h.log.Info().
		Str("request_id", requestID).
		Msg("✅ Webhook authentication verified")

	// 3. 读取请求体
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error().
			Err(err).
			Str("request_id", requestID).
			Msg("❌ Failed to read request body")
		http.Error(w, "Failed to read body", http.StatusBadRequest)
		return
	}

	h.log.Debug().
		Str("request_id", requestID).
		Int("body_size", len(body)).
		Msg("📄 Request body read successfully")

	// 🔍 日志 2: 记录原始 payload（调试用）
	h.log.Info().
		Str("request_id", requestID).
		RawJSON("payload", body).
		Msg("📦 Raw webhook payload received")

	// 4. 解析 Helius payload
	// Helius 可能发送数组或单个对象，我们需要处理两种情况
	var payload HeliusWebhookPayload
	var payloadArray []HeliusWebhookPayload

	// 先尝试解析为数组
	if err := json.Unmarshal(body, &payloadArray); err == nil && len(payloadArray) > 0 {
		// 如果是数组，取第一个元素
		payload = payloadArray[0]
		h.log.Info().
			Str("request_id", requestID).
			Int("array_length", len(payloadArray)).
			Msg("📦 Parsed webhook payload as array, using first element")
	} else {
		// 尝试解析为单个对象
		if err := json.Unmarshal(body, &payload); err != nil {
			h.log.Error().
				Err(err).
				Str("request_id", requestID).
				RawJSON("body", body).
				Msg("❌ Failed to parse webhook payload as JSON")
			http.Error(w, "Invalid JSON", http.StatusBadRequest)
			return
		}
	}

	// 🔍 日志 3: 记录解析后的 payload 结构
	h.log.Info().
		Str("request_id", requestID).
		Str("type", payload.Type).
		Int64("timestamp", payload.Timestamp).
		Msg("✅ Webhook payload parsed successfully")

	if payload.Transaction != nil {
		h.log.Info().
			Str("request_id", requestID).
			Str("signature", payload.Transaction.Signature).
			Str("txn_type", payload.Transaction.Type).
			Int("account_count", len(payload.Transaction.AccountData)).
			Msg("📝 Transaction details")
	}

	// 5. 异步处理（快速响应 Helius）
	go h.processWebhookAsync(r.Context(), payload, requestID)

	// 6. 立即返回 200（告诉 Helius 已接收）
	h.log.Info().
		Str("request_id", requestID).
		Dur("processing_time", time.Since(startTime)).
		Msg("✅ Webhook request accepted for async processing")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":      "accepted",
		"request_id":  requestID,
		"processed_at": time.Now().Format(time.RFC3339),
	})
}

// processWebhookAsync 异步处理 webhook
func (h *HeliusHandler) processWebhookAsync(ctx context.Context, payload HeliusWebhookPayload, requestID string) {
	h.log.Info().
		Str("request_id", requestID).
		Msg("🔄 Starting async webhook processing")

	if payload.Transaction == nil {
		h.log.Warn().
			Str("request_id", requestID).
			Msg("⚠️  No transaction in payload, skipping")
		return
	}

	// 尝试从 events 中获取事件（如果 Helius 解析了）
	var events []HeliusEvent

	// events 可能是数组或空对象，需要类型断言
	switch v := payload.Transaction.Events.(type) {
	case []interface{}:
		// 如果是数组，转换为 HeliusEvent
		for _, item := range v {
			if eventMap, ok := item.(map[string]interface{}); ok {
				event := HeliusEvent{
					Data: make(map[string]interface{}),
				}
				if name, ok := eventMap["name"].(string); ok {
					event.Name = name
				}
				if data, ok := eventMap["data"].(map[string]interface{}); ok {
					event.Data = data
				}
				events = append(events, event)
			}
		}
	case map[string]interface{}:
		// 如果是空对象 {}，说明 Helius 没有解析事件
		h.log.Info().
			Str("request_id", requestID).
			Str("signature", payload.Transaction.Signature).
			Msg("⚠️  Events is empty object, will fetch from chain")

		// TODO: 从链上获取交易日志并解析事件
		// 暂时跳过，返回成功
		h.log.Info().
			Str("request_id", requestID).
			Msg("✅ Webhook accepted (events parsing not implemented yet)")
		return
	}

	if len(events) == 0 {
		h.log.Warn().
			Str("request_id", requestID).
			Msg("⚠️  No events found, skipping")
		return
	}

	processedCount := 0
	for i, event := range events {
		// 🔍 日志 4: 记录每个事件的基本信息
		h.log.Info().
			Str("request_id", requestID).
			Int("event_index", i).
			Str("event_name", event.Name).
			Int("data_fields", len(event.Data)).
			Msg("🎯 Processing event")

		// 记录事件数据的原始结构（调试用）
		eventDataJSON, _ := json.MarshalIndent(event.Data, "", "  ")
		h.log.Debug().
			Str("request_id", requestID).
			Int("event_index", i).
			Str("event_name", event.Name).
			RawJSON("event_data", eventDataJSON).
			Msg("📦 Raw event data")

		if event.Name == "MarketInitialized" {
			if err := h.processMarketInitializedEvent(ctx, event.Data, requestID); err != nil {
				h.log.Error().
					Err(err).
					Str("request_id", requestID).
					Int("event_index", i).
					Msg("❌ Failed to process MarketInitialized event")
			} else {
				processedCount++
			}
		}
	}

	h.log.Info().
		Str("request_id", requestID).
		Int("processed_count", processedCount).
		Msg("✅ Async webhook processing completed")
}

// processMarketInitializedEvent 处理 MarketInitialized 事件
func (h *HeliusHandler) processMarketInitializedEvent(ctx context.Context, data map[string]interface{}, requestID string) error {
	// 1. 解析事件数据
	event, err := h.parseMarketInitializedEvent(data, requestID)
	if err != nil {
		return fmt.Errorf("failed to parse event: %w", err)
	}

	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", event.MarketID).
		Str("market_pda", event.MarketPDA).
		Str("metadata_uri", event.MetadataURI).
		Msg("🎯 MarketInitialized event parsed successfully")

	// 2. 检查市场是否已存在（幂等性检查）
	existingMarket, err := h.markets.Get(ctx, event.MarketID)
	if err == nil && existingMarket.ID != "" {
		h.log.Info().
			Str("request_id", requestID).
			Uint64("market_id", event.MarketID).
			Str("existing_id", existingMarket.ID).
			Msg("⚠️  Market already exists, skipping (idempotent)")
		return nil
	}

	// 3. 从 IPFS 获取 metadata
	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", event.MarketID).
		Str("metadata_url", event.MetadataURI).
		Msg("📡 Fetching market metadata from IPFS")

	metadata, err := h.fetchMarketMetadata(ctx, event.MetadataURI, requestID)
	if err != nil {
		h.log.Warn().
			Err(err).
			Str("request_id", requestID).
			Uint64("market_id", event.MarketID).
			Str("metadata_url", event.MetadataURI).
			Msg("⚠️  Failed to fetch metadata, using defaults")

		// 使用默认值
		metadata = &IPFSMetadata{
			Title:       fmt.Sprintf("Market %d", event.MarketID),
			Description: "",
			Category:    "other",
			ImageURL:    "",
		}
	}

	// 记录获取到的 metadata
	metadataJSON, _ := json.MarshalIndent(metadata, "", "  ")
	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", event.MarketID).
		RawJSON("metadata", metadataJSON).
		Msg("✅ Market metadata fetched")

	// 4. 合并数据创建 Market 对象
	market := h.buildMarketFromEvent(event, metadata, requestID)

	// 记录最终的 Market 对象（调试用）
	marketJSON, _ := json.MarshalIndent(market, "", "  ")
	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", market.MarketID).
		RawJSON("market", marketJSON).
		Msg("🏗️  Final market object built")

	// 5. 保存到 PostgreSQL
	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", market.MarketID).
		Msg("💾 Saving market to PostgreSQL")

	if err := h.markets.Save(ctx, market); err != nil {
		h.log.Error().
			Err(err).
			Str("request_id", requestID).
			Uint64("market_id", market.MarketID).
			Msg("❌ Failed to save market to PostgreSQL")
		return fmt.Errorf("failed to save market: %w", err)
	}

	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", market.MarketID).
		Msg("✅ Market saved to PostgreSQL successfully")

	// 6. 同时写入 Redis 缓存
	h.log.Info().
		Str("request_id", requestID).
		Uint64("market_id", market.MarketID).
		Msg("💾 Writing market to Redis cache")

	marketData := h.convertToMarketData(market)
	if err := h.marketCache.SetMarket(ctx, *marketData); err != nil {
		h.log.Error().
			Err(err).
			Str("request_id", requestID).
			Uint64("market_id", market.MarketID).
			Msg("⚠️  Failed to write market to Redis (PostgreSQL save succeeded)")
		// 不返回错误，因为 PostgreSQL 已经保存成功
	} else {
		h.log.Info().
			Str("request_id", requestID).
			Uint64("market_id", market.MarketID).
			Msg("✅ Market written to Redis successfully")
	}

	return nil
}

// parseMarketInitializedEvent 解析 MarketInitialized 事件数据
func (h *HeliusHandler) parseMarketInitializedEvent(data map[string]interface{}, requestID string) (*MarketInitializedEvent, error) {
	event := &MarketInitializedEvent{}

	// 辅助函数：从 interface{} 中提取字符串
	getString := func(key string) string {
		if val, ok := data[key]; ok {
			if str, ok := val.(string); ok {
				return str
			}
			// 如果是数字，转为字符串
			if num, ok := val.(float64); ok {
				return fmt.Sprintf("%d", int64(num))
			}
		}
		return ""
	}

	// 辅助函数：从 interface{} 中提取 uint64
	getUint64 := func(key string) uint64 {
		if val, ok := data[key]; ok {
			switch v := val.(type) {
			case float64:
				return uint64(v)
			case string:
				var num uint64
				fmt.Sscanf(v, "%d", &num)
				return num
			}
		}
		return 0
	}

	// 辅助函数：从 interface{} 中提取 int64
	getInt64 := func(key string) int64 {
		if val, ok := data[key]; ok {
			switch v := val.(type) {
			case float64:
				return int64(v)
			case string:
				var num int64
				fmt.Sscanf(v, "%d", &num)
				return num
			}
		}
		return 0
	}

	// 辅助函数：从 interface{} 中提取 int32
	getInt32 := func(key string) int32 {
		if val, ok := data[key]; ok {
			switch v := val.(type) {
			case float64:
				return int32(v)
			case string:
				var num int32
				fmt.Sscanf(v, "%d", &num)
				return num
			}
		}
		return 0
	}

	// 解析所有字段
	event.MarketID = getUint64("market_id")
	event.MarketPDA = getString("market_pda")
	event.Authority = getString("authority")
	event.MetadataURI = getString("metadata_uri")
	event.CollateralMint = getString("collateral_mint")
	event.CollateralVault = getString("collateral_vault")
	event.YesMint = getString("yes_mint")
	event.NoMint = getString("no_mint")
	event.CloseTime = getInt64("close_ts")
	event.ResolveAfterTS = getInt64("resolve_after_ts")
	event.ResolutionMode = getString("resolution_mode")
	event.OracleFeedID = getString("oracle_feed_id")
	event.OracleCondition = getString("oracle_condition")
	event.OracleTargetPriceInt = getUint64("oracle_target_price_int")
	event.OracleTargetExpo = getInt32("oracle_target_expo")

	h.log.Debug().
		Str("request_id", requestID).
		Uint64("market_id", event.MarketID).
		Msg("📋 MarketInitialized event fields parsed")

	return event, nil
}

// fetchMarketMetadata 从 IPFS 获取市场元数据
func (h *HeliusHandler) fetchMarketMetadata(ctx context.Context, url string, requestID string) (*IPFSMetadata, error) {
	h.log.Debug().
		Str("request_id", requestID).
		Str("url", url).
		Msg("📡 Starting IPFS metadata fetch")

	// 创建 HTTP 请求
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	// 设置超时
	client := &http.Client{Timeout: 10 * time.Second}

	// 发送请求
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch metadata: %w", err)
	}
	defer resp.Body.Close()

	// 检查状态码
	if resp.StatusCode != http.StatusOK {
		h.log.Warn().
			Str("request_id", requestID).
			Str("url", url).
			Int("status_code", resp.StatusCode).
			Msg("⚠️  IPFS metadata fetch returned non-200 status")
		return nil, fmt.Errorf("IPFS returned status %d", resp.StatusCode)
	}

	// 解析 JSON
	var metadata IPFSMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, fmt.Errorf("failed to decode metadata: %w", err)
	}

	return &metadata, nil
}

// buildMarketFromEvent 从事件和 metadata 构建 Market 对象
func (h *HeliusHandler) buildMarketFromEvent(event *MarketInitializedEvent, metadata *IPFSMetadata, requestID string) markets.Market {
	now := time.Now()

	market := markets.Market{
		ID:              uuid.New().String(),
		MarketID:        event.MarketID,
		MarketPDA:       event.MarketPDA,
		MetadataURL:     event.MetadataURI,
		CollateralMint:  event.CollateralMint,
		CollateralVault: event.CollateralVault,
		YesMint:         event.YesMint,
		NoMint:          event.NoMint,
		Title:           metadata.Title,
		Description:     metadata.Description,
		Category:        metadata.Category,
		ImageURL:        metadata.ImageURL,
		Status:          markets.MarketStatusOpen,
		Outcome:         markets.MarketOutcomeUndecided,
		CloseTime:       time.Unix(event.CloseTime, 0),
		Resolution: markets.ResolutionConfig{
			Mode:            markets.ResolutionMode(event.ResolutionMode),
			Authority:       event.Authority,
			OracleFeed:      event.OracleFeedID,
			OracleCondition: markets.OracleCondition(event.OracleCondition),
			OracleTarget:    event.OracleTargetPriceInt,
			ObservationTime: time.Unix(event.ResolveAfterTS, 0),
		},
		CreatedAt: now,
		UpdatedAt: now,
	}

	return market
}

// convertToMarketData 将 Market 转换为 Redis 存储格式
func (h *HeliusHandler) convertToMarketData(market markets.Market) *cache.MarketData {
	return &cache.MarketData{
		ID:                      market.ID,
		MarketID:                market.MarketID,
		MarketPDA:               market.MarketPDA,
		MetadataURL:             market.MetadataURL,
		CollateralMint:          market.CollateralMint,
		CollateralVault:         market.CollateralVault,
		YesMint:                 market.YesMint,
		NoMint:                  market.NoMint,
		Title:                   market.Title,
		Description:             market.Description,
		Category:                market.Category,
		ImageURL:                market.ImageURL,
		Status:                  string(market.Status),
		Outcome:                 string(market.Outcome),
		ResolutionMode:          string(market.Resolution.Mode),
		ResolutionAuthority:     market.Resolution.Authority,
		OracleFeed:              market.Resolution.OracleFeed,
		OracleCondition:         string(market.Resolution.OracleCondition),
		OracleTargetPrice:       int64(market.Resolution.OracleTarget),
		CloseTime:               market.CloseTime.Unix(),
		OracleObservationTime:   market.Resolution.ObservationTime.Unix(),
		CreatedAt:               market.CreatedAt.Unix(),
		UpdatedAt:               market.UpdatedAt.Unix(),
	}
}

// HandleWebhookFastHTTP 快速 HTTP 处理（使用 fasthttp，可选）
func (h *HeliusHandler) HandleWebhookFastHTTP(ctx *fasthttp.RequestCtx) {
	requestID := uuid.New().String()
	startTime := time.Now()

	// 🔍 日志 1: 记录请求基本信息
	h.log.Info().
		Str("request_id", requestID).
		Str("method", string(ctx.Method())).
		Str("path", string(ctx.Path())).
		Str("remote_addr", ctx.RemoteAddr().String()).
		Msg("📥 Received Helius webhook request (fasthttp)")

	// 1. 验证请求方法
	if string(ctx.Method()) != "POST" {
		h.log.Warn().
			Str("request_id", requestID).
			Str("method", string(ctx.Method())).
			Msg("❌ Invalid method, only POST allowed")
		ctx.Error("Method not allowed", fasthttp.StatusMethodNotAllowed)
		return
	}

	// 2. 验证 Auth Header
	authHeader := string(ctx.Request.Header.Peek("Authorization"))

	// 支持两种格式：
	// 1. "Bearer <token>" (标准格式)
	// 2. "<token>" (Helius 直接发送 token)
	var receivedToken string
	if len(authHeader) > 7 && authHeader[:7] == "Bearer " {
		receivedToken = authHeader[7:]
	} else {
		receivedToken = authHeader
	}

	h.log.Debug().
		Str("request_id", requestID).
		Str("auth_header", authHeader).
		Str("expected_token", h.webhookSecret).
		Msg("🔐 Verifying webhook authentication")

	if receivedToken != h.webhookSecret {
		h.log.Error().
			Str("request_id", requestID).
			Str("received_token", receivedToken).
			Msg("❌ Invalid webhook authentication")
		ctx.Error("Unauthorized", fasthttp.StatusUnauthorized)
		return
	}

	h.log.Info().
		Str("request_id", requestID).
		Msg("✅ Webhook authentication verified")

	// 3. 读取请求体
	body := ctx.PostBody()

	h.log.Debug().
		Str("request_id", requestID).
		Int("body_size", len(body)).
		Msg("📄 Request body read successfully")

	// 🔍 日志 2: 记录原始 payload
	h.log.Info().
		Str("request_id", requestID).
		RawJSON("payload", body).
		Msg("📦 Raw webhook payload received")

	// 4. 解析 Helius payload
	// Helius 可能发送数组或单个对象，我们需要处理两种情况
	var payload HeliusWebhookPayload
	var payloadArray []HeliusWebhookPayload

	// 先尝试解析为数组
	if err := json.Unmarshal(body, &payloadArray); err == nil && len(payloadArray) > 0 {
		// 如果是数组，取第一个元素
		payload = payloadArray[0]
		h.log.Info().
			Str("request_id", requestID).
			Int("array_length", len(payloadArray)).
			Msg("📦 Parsed webhook payload as array, using first element")
	} else {
		// 尝试解析为单个对象
		if err := json.Unmarshal(body, &payload); err != nil {
			h.log.Error().
				Err(err).
				Str("request_id", requestID).
				RawJSON("body", body).
				Msg("❌ Failed to parse webhook payload as JSON")
			ctx.Error("Invalid JSON", fasthttp.StatusBadRequest)
			return
		}
	}

	// 🔍 日志 3: 记录解析后的 payload 结构
	h.log.Info().
		Str("request_id", requestID).
		Str("type", payload.Type).
		Int64("timestamp", payload.Timestamp).
		Msg("✅ Webhook payload parsed successfully")

	if payload.Transaction != nil {
		h.log.Info().
			Str("request_id", requestID).
			Str("signature", payload.Transaction.Signature).
			Str("txn_type", payload.Transaction.Type).
			Int("account_count", len(payload.Transaction.AccountData)).
			Msg("📝 Transaction details")
	}

	// 5. 异步处理
	go h.processWebhookAsync(context.Background(), payload, requestID)

	// 6. 立即返回 200
	h.log.Info().
		Str("request_id", requestID).
		Dur("processing_time", time.Since(startTime)).
		Msg("✅ Webhook request accepted for async processing")

	ctx.SetContentType("application/json")
	ctx.SetStatusCode(fasthttp.StatusOK)
	_ = json.NewEncoder(ctx).Encode(map[string]interface{}{
		"status":      "accepted",
		"request_id":  requestID,
		"processed_at": time.Now().Format(time.RFC3339),
	})
}
