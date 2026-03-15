use anchor_lang::prelude::*;

#[account]
#[derive(InitSpace)]
pub struct Position {
    pub market_id: Pubkey,
    pub owner: Pubkey,
    pub yes_share: u64,
    pub no_share: u64,
    pub total: u64,
    pub claimed: bool,
    pub bump: u8,
}
