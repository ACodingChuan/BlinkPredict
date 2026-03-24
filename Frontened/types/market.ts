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
  oracle_observation_time?: string;
}

export interface Market {
  id: string;
  market_id: string;
  market_pda: string;
  metadata_url: string;
  collateral_mint: string;
  collateral_vault: string;
  yes_mint: string;
  no_mint: string;
  title: string;
  description: string;
  category: string;
  image_url: string;
  status: MarketStatus;
  outcome: MarketOutcome;
  resolution: ResolutionConfig;
  close_time: string;
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
  rules?: string | string[];
  resolution?: {
    mode?: string;
    authority?: string;
    oracle_feed_id?: string;
    oracle_condition?: string;
    oracle_target_price?: string;
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
}

export interface OpenOrdersResponse {
  orders: OpenOrderItem[];
  matching_enabled: boolean;
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

export interface MarketDepthSocketMessage {
  market_id: string;
  updated_at: string;
  source_cmd_seq?: string;
  levels: Array<{ side: number; price_tick: number; total_volume: number }>;
}

export interface MarketTradeSocketMessage {
  market_id: string;
  trade_id: string;
  maker_order_id: string;
  taker_order_id: string;
  maker_wallet_address: string;
  taker_wallet_address: string;
  price_tick: string;
  match_qty: string;
  executed_at: string;
}

export interface UserOrderSocketMessage {
  market_id: string;
  wallet_address: string;
  order: {
    id: string;
    side?: string;
    outcome?: string;
    price?: string;
    quantity?: string;
    status: number;
    refund_amount?: string;
    updated_at: string;
  };
}
