use anchor_lang::prelude::*;
#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum Side {
    Buy = 0,
    Sell = 1,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum Outcome {
    Yes = 0,
    No = 1,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum OrderType {
    Limit = 0,
    Market = 1,
}

#[derive(Clone, Copy, Debug, AnchorDeserialize, AnchorSerialize, InitSpace, PartialEq, Eq)]
pub enum SettleBranch {
    MatchAndMint = 0,
    Transfer = 1,
    MergeAndBurn = 2,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct OrderIntentV1 {
    pub version: u8,
    pub program_id: Pubkey,
    pub market: Pubkey,
    pub user: Pubkey,
    pub nonce: u64,
    pub side: Side,
    pub outcome: Outcome,
    pub order_type: OrderType,
    pub limit_price: u64,
    pub total_amount: u64,
    pub expiry_ts: i64,
}

#[account(zero_copy)]
#[repr(C)]
#[derive(Debug)]
pub struct OrderState {
    pub owner: Pubkey,
    pub nonce: u64,
    pub order_hash: [u8; 32],
    pub total_amount: u64,
    pub filled_amount: u64,
    pub paid_cash: u64,
    pub paid_creator_fee: u64,
    pub paid_platform_fee: u64,
    pub cash_remainder: u8,
    pub canceled: u8,
    pub bump: u8,
    pub _reserved: [u8; 5],
}

impl OrderState {
    pub const INIT_SPACE: usize = core::mem::size_of::<Self>();

    pub fn is_empty_for_close(&self) -> bool {
        self.filled_amount >= self.total_amount || self.canceled != 0
    }
}

#[event]
pub struct OrderStateClosed {
    pub owner: Pubkey,
    pub market: Pubkey,
    pub nonce: u64,
}
