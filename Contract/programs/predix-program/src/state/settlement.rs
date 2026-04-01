use anchor_lang::prelude::*;

use crate::state::{OrderIntentV1, SettleBranch};

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct FillIndexPair {
    pub maker_idx: u16,
    pub taker_idx: u16,
    pub fill_amount: u64,
    pub fill_price: u64,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct SettleMatchBatchArgs {
    pub orders: Vec<OrderIntentV1>,
    pub fills: Vec<FillIndexPair>,
}

#[event]
pub struct SettlementBatchAccepted {
    pub market: Pubkey,
    pub order_count: u16,
    pub fill_count: u16,
}

#[event]
pub struct MatchSettled {
    pub market: Pubkey,
    pub branch: SettleBranch,
    pub maker: Pubkey,
    pub taker: Pubkey,
    pub fill_amount: u64,
    pub fill_price: u64,
}
