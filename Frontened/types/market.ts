export type ResolutionMode = "creator" | "pyth";
export type OracleCondition = "gt" | "gte" | "lt" | "lte";
export type MarketStatus = "open" | "resolved";
export type MarketOutcome = "yes" | "no" | "undecided";

export interface ResolutionConfig {
  mode: ResolutionMode;
  authority?: string;
  oracle_feed?: string;
  oracle_condition?: OracleCondition;
  oracle_target_price?: number;
  oracle_target_expo?: number;
  oracle_observation_time?: string;
}

export interface Market {
  id: string;
  market_id: string;
  market_pda: string;
  metadata_cid?: string;
  metadata_url: string;
  collateral_mint?: string;
  title: string;
  description: string;
  category?: string;
  image_url: string;
  status: MarketStatus;
  outcome: MarketOutcome;
  resolution: ResolutionConfig;
  close_time: string;
  resolve_after_time?: string;
  claim_deadline_time?: string;
  resolved_at?: string | null;
  created_at: string;
  updated_at: string;
}

export interface MarketsResponse {
  markets: Market[];
}

export interface MarketResponse {
  market: Market;
}

export interface MarketMetadataDoc {
  title?: string;
  description?: string;
  image?: string;
  image_url?: string;
  close_time?: string;
  settle_time?: string;
  resolve_after_time?: string;
  claim_deadline_time?: string;
  rules?: string | string[];
  resolution?: {
    mode?: string;
    authority?: string;
    oracle_feed_id?: string;
    oracle_condition?: string;
    oracle_target_price?: string;
    oracle_target_expo?: string;
  };
}

export interface TransactionEnvelope {
  tx_message: string;
  message: string;
  disabled?: boolean;
  code?: string;
}

export interface PlaceOrderCommandResponse {
  message: string;
  command_id: string;
  market_id: string;
  order_id: string;
  idempotency_key: string;
}

export interface OpenOrderItem {
  id: string;
  side?: string;
  outcome?: string;
  price?: string;
  quantity?: string;
  status?: string;
}

export interface OpenOrdersResponse {
  orders: OpenOrderItem[];
  matching_enabled: boolean;
}

export interface PositionResponse {
  market_id: string;
  wallet_address: string;
  yes_free_lots: string;
  yes_locked_lots: string;
  yes_pending_lots: string;
  no_free_lots: string;
  no_locked_lots: string;
  no_pending_lots: string;
  collateral_locked_units: string;
}

export interface TradeItem {
  id: string;
  price?: string;
  quantity?: string;
  executed_at?: string;
}

export interface TradesResponse {
  trades: TradeItem[];
  matching_enabled: boolean;
}

export type PriceHistoryRange = "1H" | "6H" | "1D" | "1W" | "1M" | "ALL";

export interface PricePoint {
  timestamp: string;
  price: string;
  quantity?: string;
}

export interface PriceHistoryResponse {
  range: PriceHistoryRange;
  points: PricePoint[];
}

export interface OrderbookSnapshot {
  bids: Array<{ price: string; total_volume: string }>;
  asks: Array<{ price: string; total_volume: string }>;
  best_bid_price?: string;
  best_ask_price?: string;
  matching_enabled: boolean;
}

export interface WSDepthLevel {
  side: "bid" | "ask";
  price_tick: number;
  total_volume: number;
}

export interface WSOrderbookLevel {
  price: string;
  total_volume: string;
}

export interface WSPublicTrade {
  trade_id: string;
  price_tick: string;
  fill_amount: string;
  executed_at: string;
}

export interface WSPublicPricePoint {
  timestamp: string;
  price: string;
  quantity?: string;
}

export interface MarketSnapshotSocketMessage {
  type: "market.snapshot";
  market_id: string;
  seq: string;
  ts: string;
  payload: {
    matching_enabled: boolean;
    orderbook: {
      bids: WSOrderbookLevel[];
      asks: WSOrderbookLevel[];
      best_bid_price?: string;
      best_ask_price?: string;
    };
    trades: WSPublicTrade[];
    price_history: WSPublicPricePoint[];
  };
}

export interface MarketDeltaSocketMessage {
  type: "market.delta";
  market_id: string;
  seq: string;
  ts: string;
  payload: {
    depth_levels?: WSDepthLevel[];
    trades?: WSPublicTrade[];
    price_points?: WSPublicPricePoint[];
  };
}

export type MarketPublicSocketMessage = MarketSnapshotSocketMessage | MarketDeltaSocketMessage;
