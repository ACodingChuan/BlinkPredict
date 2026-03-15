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
  market_id: number;
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

export interface TransactionEnvelope {
  tx_message: string;
  message: string;
  disabled?: boolean;
  code?: string;
}

export interface OrderbookSnapshot {
  yes: { bids: Array<{ price: string; quantity: string }>; asks: Array<{ price: string; quantity: string }> };
  no: { bids: Array<{ price: string; quantity: string }>; asks: Array<{ price: string; quantity: string }> };
  matching_enabled: boolean;
}
