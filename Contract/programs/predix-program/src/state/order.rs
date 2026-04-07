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

pub const ORDER_FLAG_SIDE_SELL: u8 = 1 << 0;
pub const ORDER_FLAG_OUTCOME_NO: u8 = 1 << 1;
pub const ORDER_FLAG_TYPE_MARKET: u8 = 1 << 2;
pub const ORDER_FLAG_CANCELED: u8 = 1 << 3;

#[derive(AnchorSerialize, AnchorDeserialize, Clone, Debug)]
pub struct OrderIntentV1 {
    pub program_id: Pubkey,
    pub market: Pubkey,
    pub user: Pubkey,
    pub nonce: u64,
    pub side: Side,
    pub outcome: Outcome,
    pub order_type: OrderType,
    pub limit_price: u8,
    pub total_amount: u64,
    pub expiry_ts: u32,
}

#[account(zero_copy)]
#[repr(C)]
#[derive(Debug)]
pub struct OrderState {
    pub owner: Pubkey,
    pub nonce: u64,
    pub total_amount: u64,
    pub filled_amount: u64,
    pub paid_cash: u64,
    pub expiry_ts: u32,
    pub limit_price: u8,
    pub flags: u8,
    pub cash_remainder: u8,
    pub bump: u8,
    pub _reserved: [u8; 8],
}

impl OrderState {
    pub const INIT_SPACE: usize = core::mem::size_of::<Self>();

    pub fn is_empty_for_close(&self) -> bool {
        self.filled_amount >= self.total_amount || self.is_canceled()
    }

    pub fn side(&self) -> Side {
        if self.flags & ORDER_FLAG_SIDE_SELL != 0 {
            Side::Sell
        } else {
            Side::Buy
        }
    }

    pub fn outcome(&self) -> Outcome {
        if self.flags & ORDER_FLAG_OUTCOME_NO != 0 {
            Outcome::No
        } else {
            Outcome::Yes
        }
    }

    pub fn order_type(&self) -> OrderType {
        if self.flags & ORDER_FLAG_TYPE_MARKET != 0 {
            OrderType::Market
        } else {
            OrderType::Limit
        }
    }

    pub fn is_canceled(&self) -> bool {
        self.flags & ORDER_FLAG_CANCELED != 0
    }
}

#[event]
pub struct OrderStateClosed {
    pub owner: Pubkey,
    pub market: Pubkey,
    pub nonce: u64,
}
