use anchor_lang::prelude::*;

use crate::state::SettleBranch;

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct OrderSlot {
    pub user_idx: u8,
    pub cold_witness_idx: u8,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct ColdOrderWitness {
    pub nonce: u64,
    pub total_amount: u64,
    pub expiry_ts: u32,
    pub limit_price: u8,
    pub flags: u8,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct FillIndexPair {
    pub maker_idx: u8,
    pub taker_idx: u8,
    pub fill_amount: u32,
    pub fill_price: u8,
}

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct SettleMatchBatchArgs {
    pub user_count: u8,
    pub orders: Vec<OrderSlot>,
    pub cold_witnesses: Vec<ColdOrderWitness>,
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
