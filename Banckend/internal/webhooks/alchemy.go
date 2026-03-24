package webhooks

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog"

	"blinkpredict/banckend/internal/cache"
	"blinkpredict/banckend/internal/markets"
)

// AlchemyHandler Alchemy webhook 处理器
type AlchemyHandler struct {
	markets     markets.Repository
	marketCache *cache.MarketCache
	log         *zerolog.Logger
	signingKey  string
	programID   string
	rpcURL      string
}

// NewAlchemyHandler 创建 Alchemy webhook 处理器
func NewAlchemyHandler(
	marketsRepo markets.Repository,
	marketCache *cache.MarketCache,
	logger *zerolog.Logger,
	signingKey string,
	programID string,
	rpcURL string,
) *AlchemyHandler {
	return &AlchemyHandler{
		markets:    marketsRepo,
		marketCache: marketCache,
		log:        logger,
		signingKey: signingKey,
		programID:  programID,
		rpcURL:     rpcURL,
	}
}

// alchemyWebhookEvent Alchemy webhook payload 结构
type alchemyWebhookEvent struct {
	WebhookID string                 `json:"webhookId"`
	ID        string                 `json:"id"`
	CreatedAt time.Time              `json:"createdAt"`
	Type      string                 `json:"type"`
	Event     map[string]interface{} `json:"event"`
}

// HandleWebhook 处理 Alchemy webhook 请求
func (h *AlchemyHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.New().String()

	h.log.Info().
		Str("request_id", requestID).
		Str("method", r.Method).
		Str("path", r.URL.Path).
		Msg("📥 Received Alchemy webhook request")

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// 读取 body（必须在验签前读取原始 body）
	body, err := io.ReadAll(r.Body)
	if err != nil {
		h.log.Error().Err(err).Str("request_id", requestID).Msg("❌ Failed to read body")
		http.Error(w, "Failed to read body", http.StatusInternalServerError)
		return
	}

	// 验证 HMAC-SHA256 签名
	signature := r.Header.Get("x-alchemy-signature")
	if !h.isValidSignature(body, signature) {
		h.log.Error().
			Str("request_id", requestID).
			Str("signature", signature).
			Msg("❌ Invalid Alchemy signature")
		http.Error(w, "Unauthorized", http.StatusForbidden)
		return
	}

	h.log.Info().Str("request_id", requestID).Msg("✅ Alchemy signature verified")

	// 解析 payload
	var event alchemyWebhookEvent
	if err := json.Unmarshal(body, &event); err != nil {
		h.log.Error().Err(err).Str("request_id", requestID).Msg("❌ Failed to parse payload")
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	h.log.Info().
		Str("request_id", requestID).
		Str("webhook_id", event.WebhookID).
		Str("type", event.Type).
		Msg("✅ Alchemy payload parsed")

	// 异步处理
	go h.processAsync(context.Background(), event, requestID)

	// 立即返回 200
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":     "accepted",
		"request_id": requestID,
	})
}

// isValidSignature 验证 HMAC-SHA256 签名
func (h *AlchemyHandler) isValidSignature(body []byte, signature string) bool {
	mac := hmac.New(sha256.New, []byte(h.signingKey))
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	return expected == signature
}

// processAsync 异步处理 webhook 事件
func (h *AlchemyHandler) processAsync(ctx context.Context, event alchemyWebhookEvent, requestID string) {
	h.log.Info().Str("request_id", requestID).Msg("🔄 Processing Alchemy webhook async")

	// 从 event.Event 中提取交易列表
	txList, ok := extractTxList(event.Event)
	if !ok || len(txList) == 0 {
		h.log.Warn().Str("request_id", requestID).Msg("⚠️  No transactions in event")
		return
	}

	for _, tx := range txList {
		sig := extractString(tx, "signature")
		if sig == "" {
			continue
		}

		// 从 meta.log_messages 中提取 Program data
		logMessages := extractLogMessages(tx)
		programData := findProgramData(logMessages, h.programID)
		if programData == "" {
			h.log.Debug().
				Str("request_id", requestID).
				Str("signature", sig).
				Msg("⚠️  No program data found in logs")
			continue
		}

		h.log.Info().
			Str("request_id", requestID).
			Str("signature", sig).
			Str("program_data", programData[:min(len(programData), 40)]+"...").
			Msg("🎯 Found program data, decoding Anchor event")

		// 解码 Anchor 事件
		marketEvent, err := decodeMarketInitializedEvent(programData)
		if err != nil {
			h.log.Debug().
				Err(err).
				Str("request_id", requestID).
				Str("signature", sig).
				Msg("⚠️  Not a MarketInitialized event, skipping")
			continue
		}

		h.log.Info().
			Str("request_id", requestID).
			Uint64("market_id", marketEvent.MarketID).
			Str("metadata_uri", marketEvent.MetadataURI).
			Msg("🎯 MarketInitialized event decoded!")

		if err := h.processMarketInitialized(ctx, marketEvent, sig, requestID); err != nil {
			h.log.Error().Err(err).Str("request_id", requestID).Msg("❌ Failed to process market")
		}
	}
}

// processMarketInitialized 保存市场到数据库
func (h *AlchemyHandler) processMarketInitialized(ctx context.Context, event *MarketInitializedEvent, sig string, requestID string) error {
	// 幂等性检查
	existing, err := h.markets.Get(ctx, event.MarketID)
	if err == nil && existing.ID != "" {
		h.log.Info().Uint64("market_id", event.MarketID).Msg("⚠️  Market already exists, skipping")
		return nil
	}

	// 从 IPFS 获取 metadata
	metadata, err := fetchIPFSMetadata(ctx, event.MetadataURI)
	if err != nil {
		h.log.Warn().Err(err).Str("metadata_uri", event.MetadataURI).Msg("⚠️  Failed to fetch metadata, using defaults")
		metadata = &IPFSMetadata{
			Title:    fmt.Sprintf("Market %d", event.MarketID),
			Category: "other",
		}
	}

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

	if err := h.markets.Save(ctx, market); err != nil {
		return fmt.Errorf("failed to save market: %w", err)
	}
	h.log.Info().Uint64("market_id", market.MarketID).Msg("✅ Market saved to PostgreSQL")

	// 写入 Redis 缓存
	marketData := convertMarketToCache(market)
	if err := h.marketCache.SetMarket(ctx, *marketData); err != nil {
		h.log.Warn().Err(err).Msg("⚠️  Failed to write to Redis")
	} else {
		h.log.Info().Uint64("market_id", market.MarketID).Msg("✅ Market written to Redis")
	}

	return nil
}

// fetchIPFSMetadata 从 IPFS 获取 metadata（复用逻辑）
func fetchIPFSMetadata(ctx context.Context, uri string) (*IPFSMetadata, error) {
	// 将 ipfs:// 转换为 https://ipfs.io/ipfs/
	url := uri
	if strings.HasPrefix(uri, "ipfs://") {
		url = "https://ipfs.io/ipfs/" + uri[7:]
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("IPFS returned %d", resp.StatusCode)
	}

	var metadata IPFSMetadata
	if err := json.NewDecoder(resp.Body).Decode(&metadata); err != nil {
		return nil, err
	}
	return &metadata, nil
}

// convertMarketToCache 将 Market 转换为 Redis 格式
func convertMarketToCache(market markets.Market) *cache.MarketData {
	return &cache.MarketData{
		ID:                    market.ID,
		MarketID:              market.MarketID,
		MarketPDA:             market.MarketPDA,
		MetadataURL:           market.MetadataURL,
		CollateralMint:        market.CollateralMint,
		CollateralVault:       market.CollateralVault,
		YesMint:               market.YesMint,
		NoMint:                market.NoMint,
		Title:                 market.Title,
		Description:           market.Description,
		Category:              market.Category,
		ImageURL:              market.ImageURL,
		Status:                string(market.Status),
		Outcome:               string(market.Outcome),
		ResolutionMode:        string(market.Resolution.Mode),
		ResolutionAuthority:   market.Resolution.Authority,
		OracleFeed:            market.Resolution.OracleFeed,
		OracleCondition:       string(market.Resolution.OracleCondition),
		OracleTargetPrice:     int64(market.Resolution.OracleTarget),
		CloseTime:             market.CloseTime.Unix(),
		OracleObservationTime: market.Resolution.ObservationTime.Unix(),
		CreatedAt:             market.CreatedAt.Unix(),
		UpdatedAt:             market.UpdatedAt.Unix(),
	}
}

// --- Anchor 事件解码 ---

// anchorDiscriminator 计算 Anchor 事件的 discriminator（前8字节）
// Anchor 事件 discriminator = sha256("event:<EventName>")[0:8]
func anchorEventDiscriminator(name string) []byte {
	h := sha256.New()
	h.Write([]byte("event:" + name))
	return h.Sum(nil)[:8]
}

// decodeMarketInitializedEvent 从 base64 Program data 解码 MarketInitialized 事件
func decodeMarketInitializedEvent(programData string) (*MarketInitializedEvent, error) {
	raw, err := base64.StdEncoding.DecodeString(programData)
	if err != nil {
		return nil, fmt.Errorf("base64 decode failed: %w", err)
	}

	// Anchor 事件格式: [8字节discriminator][事件数据]
	if len(raw) < 8 {
		return nil, fmt.Errorf("data too short: %d bytes", len(raw))
	}

	// 验证 discriminator
	expected := anchorEventDiscriminator("MarketInitialized")
	for i := 0; i < 8; i++ {
		if raw[i] != expected[i] {
			return nil, fmt.Errorf("discriminator mismatch")
		}
	}

	// 解析事件字段（Borsh 编码，小端序）
	data := raw[8:]
	event := &MarketInitializedEvent{}
	offset := 0

	// market_id: u64 (8 bytes)
	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for market_id")
	}
	event.MarketID = readU64LE(data[offset:])
	offset += 8

	// market_pda: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for market_pda")
	}
	event.MarketPDA = base58Encode(data[offset : offset+32])
	offset += 32

	// authority: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for authority")
	}
	event.Authority = base58Encode(data[offset : offset+32])
	offset += 32

	// collateral_mint: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for collateral_mint")
	}
	event.CollateralMint = base58Encode(data[offset : offset+32])
	offset += 32

	// collateral_vault: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for collateral_vault")
	}
	event.CollateralVault = base58Encode(data[offset : offset+32])
	offset += 32

	// metadata_uri: String (4 bytes length prefix + bytes)
	if offset+4 > len(data) {
		return nil, fmt.Errorf("data too short for metadata_uri length")
	}
	strLen := int(readU32LE(data[offset:]))
	offset += 4
	if offset+strLen > len(data) {
		return nil, fmt.Errorf("data too short for metadata_uri content")
	}
	event.MetadataURI = string(data[offset : offset+strLen])
	offset += strLen

	// yes_mint: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for yes_mint")
	}
	event.YesMint = base58Encode(data[offset : offset+32])
	offset += 32

	// no_mint: Pubkey (32 bytes)
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for no_mint")
	}
	event.NoMint = base58Encode(data[offset : offset+32])
	offset += 32

	// close_ts: i64 (8 bytes)
	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for close_ts")
	}
	event.CloseTime = int64(readU64LE(data[offset:]))
	offset += 8

	// resolve_after_ts: i64 (8 bytes)
	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for resolve_after_ts")
	}
	event.ResolveAfterTS = int64(readU64LE(data[offset:]))
	offset += 8

	// resolution_mode: u8 (1 byte) — 0=Creator, 1=Pyth
	if offset+1 > len(data) {
		return nil, fmt.Errorf("data too short for resolution_mode")
	}
	if data[offset] == 0 {
		event.ResolutionMode = "Creator"
	} else {
		event.ResolutionMode = "Pyth"
	}
	offset++

	// oracle_feed_id: [u8; 32]
	if offset+32 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_feed_id")
	}
	event.OracleFeedID = hex.EncodeToString(data[offset : offset+32])
	offset += 32

	// oracle_condition: u8 (1 byte) — 0=GT, 1=GTE, 2=LT, 3=LTE
	if offset+1 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_condition")
	}
	conditions := []string{"GreaterThan", "GreaterThanOrEqual", "LessThan", "LessThanOrEqual"}
	if int(data[offset]) < len(conditions) {
		event.OracleCondition = conditions[data[offset]]
	}
	offset++

	// oracle_target_price_int: u64 (8 bytes)
	if offset+8 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_target_price_int")
	}
	event.OracleTargetPriceInt = readU64LE(data[offset:])
	offset += 8

	// oracle_target_expo: i32 (4 bytes)
	if offset+4 > len(data) {
		return nil, fmt.Errorf("data too short for oracle_target_expo")
	}
	event.OracleTargetExpo = int32(readU32LE(data[offset:]))

	return event, nil
}

// --- 辅助函数 ---

func readU64LE(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56
}

func readU32LE(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// base58Encode 将字节数组编码为 base58 字符串（Solana 地址格式）
func base58Encode(b []byte) string {
	const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"
	result := make([]byte, 0, 44)
	num := make([]byte, len(b))
	copy(num, b)

	// 计算前导零
	leadingZeros := 0
	for _, v := range num {
		if v != 0 {
			break
		}
		leadingZeros++
	}

	// 转换为 base58
	for len(num) > 0 {
		remainder := 0
		newNum := make([]byte, 0, len(num))
		for _, v := range num {
			digit := remainder*256 + int(v)
			if len(newNum) > 0 || digit/58 > 0 {
				newNum = append(newNum, byte(digit/58))
			}
			remainder = digit % 58
		}
		result = append(result, alphabet[remainder])
		num = newNum
	}

	for i := 0; i < leadingZeros; i++ {
		result = append(result, alphabet[0])
	}

	// 反转
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

// extractTxList 从 Alchemy event map 中提取交易列表
func extractTxList(event map[string]interface{}) ([]map[string]interface{}, bool) {
	txRaw, ok := event["transaction"]
	if !ok {
		return nil, false
	}
	txArr, ok := txRaw.([]interface{})
	if !ok {
		return nil, false
	}
	result := make([]map[string]interface{}, 0, len(txArr))
	for _, item := range txArr {
		if m, ok := item.(map[string]interface{}); ok {
			result = append(result, m)
		}
	}
	return result, true
}

// extractLogMessages 从交易 map 中提取 log_messages
func extractLogMessages(tx map[string]interface{}) []string {
	metaRaw, ok := tx["meta"]
	if !ok {
		return nil
	}
	metaArr, ok := metaRaw.([]interface{})
	if !ok || len(metaArr) == 0 {
		return nil
	}
	meta, ok := metaArr[0].(map[string]interface{})
	if !ok {
		return nil
	}
	logsRaw, ok := meta["log_messages"]
	if !ok {
		return nil
	}
	logsArr, ok := logsRaw.([]interface{})
	if !ok {
		return nil
	}
	logs := make([]string, 0, len(logsArr))
	for _, l := range logsArr {
		if s, ok := l.(string); ok {
			logs = append(logs, s)
		}
	}
	return logs
}

// findProgramData 从日志中找到 Program data 行并返回 base64 数据
func findProgramData(logs []string, programID string) string {
	inProgram := false
	for _, line := range logs {
		if strings.Contains(line, programID+" invoke") {
			inProgram = true
		}
		if inProgram && strings.HasPrefix(line, "Program data: ") {
			return strings.TrimPrefix(line, "Program data: ")
		}
		if inProgram && strings.Contains(line, programID+" success") {
			inProgram = false
		}
	}
	return ""
}

func extractString(m map[string]interface{}, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
