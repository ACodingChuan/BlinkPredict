use anchor_lang::prelude::*;

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum MarketStatus {
    Trading = 0,
    Resolved = 1,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum MarketOutcome {
    Undecided = 0,
    Yes = 1,
    No = 2,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum ResolutionMode {
    Creator = 0,
    Pyth = 1,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum OracleCondition {
    GreaterThan = 0,
    GreaterThanOrEqual = 1,
    LessThan = 2,
    LessThanOrEqual = 3,
}

#[account]
#[derive(InitSpace)]
pub struct MarketState {
    pub authority: Pubkey,
    pub metadata_cid: [u8; 96],
    pub market_id: u64,
    pub status: MarketStatus,
    pub outcome: MarketOutcome,
    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub resolved_at: i64,
    pub oracle_feed_id: [u8; 32],
    pub oracle_condition: OracleCondition,
    pub oracle_target_price_int: u64,
    pub oracle_target_expo: i32,
    pub creator_fee_bps: u16,
    pub platform_fee_bps: u16,
    pub creator_unclaimed_fee: u64,
    pub platform_unclaimed_fee: u64,
    pub total_yes_open_interest: u64,
    pub total_no_open_interest: u64,
    pub total_matched_amount: u64,
    pub resolution_mode: ResolutionMode,
    pub bump: u8,
}

#[event]
pub struct MarketCreated {
    pub market_id: u64,
    pub market_pda: Pubkey,
    pub authority: Pubkey,
    pub metadata_cid: [u8; 96],
    pub close_ts: i64,
    pub resolve_after_ts: i64,
    pub claim_deadline_ts: i64,
    pub resolution_mode: ResolutionMode,
    pub oracle_feed_id: [u8; 32],
    pub oracle_condition: OracleCondition,
    pub oracle_target_price_int: u64,
    pub oracle_target_expo: i32,
}

#[event]
pub struct MarketClosed {
    pub market: Pubkey,
    pub closed_at: i64,
}
