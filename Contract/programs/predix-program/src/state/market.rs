use anchor_lang::prelude::*;

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum MarketOutcome {
    Yes = 0,
    No = 1,
    Undecided = 2,
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
pub struct Market {
    pub authority: Pubkey,
    #[max_len(256)]
    pub metadata_url: String,
    pub collateral_vault: Pubkey,
    pub collateral_mint: Pubkey,
    pub yes_mint: Pubkey,
    pub no_mint: Pubkey,
    pub market_id: u64,
    pub yes_total: u64,
    pub no_total: u64,
    pub outcome: MarketOutcome,
    pub expiration_timestamp: i64,
    pub is_settled: bool,
    pub resolution_mode: ResolutionMode,
    pub resolution_authority: Pubkey,
    pub oracle_feed: Pubkey,
    pub oracle_condition: OracleCondition,
    pub oracle_target_price: u64,
    pub oracle_observation_time: i64,
    pub resolved_at: i64,
    pub bump: u8,
}

#[event]
pub struct MarketInitialized {
    pub market_id: u64,
    pub market_pda: Pubkey,
    pub authority: Pubkey,
    pub collateral_mint: Pubkey,
    pub collateral_vault: Pubkey,
    pub metadata_url: String,
    pub yes_mint: Pubkey,
    pub no_mint: Pubkey,
    pub expiration_timestamp: i64,
    pub resolution_mode: ResolutionMode,
    pub resolution_authority: Pubkey,
    pub oracle_feed: Pubkey,
    pub oracle_condition: OracleCondition,
    pub oracle_target_price: u64,
    pub oracle_observation_time: i64,
}

#[event]
pub struct TokensSplit {
    pub market_id: u64,
    pub market: Pubkey,
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct TokensMerged {
    pub market_id: u64,
    pub market: Pubkey,
    pub user: Pubkey,
    pub amount: u64,
}

#[event]
pub struct MarketResolved {
    pub market_id: u64,
    pub market: Pubkey,
    pub resolution_mode: ResolutionMode,
    pub outcome: MarketOutcome,
    pub resolved_at: i64,
}

#[event]
pub struct RewardsClaimed {
    pub market_id: u64,
    pub market: Pubkey,
    pub user: Pubkey,
    pub amount: u64,
    pub outcome: MarketOutcome,
}
