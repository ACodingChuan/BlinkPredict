use anchor_lang::prelude::*;

#[account(zero_copy)]
#[repr(C)]
#[derive(Debug)]
pub struct UserLedger {
    pub owner: Pubkey,
    pub available_usdc: u64,
    pub cancel_all_before_ts: i64,
    pub bump: u8,
    pub _reserved: [u8; 7],
}

impl UserLedger {
    pub const INIT_SPACE: usize = core::mem::size_of::<Self>();
}

#[event]
pub struct UserLedgerInitialized {
    pub owner: Pubkey,
}

#[event]
pub struct DepositSettled {
    pub owner: Pubkey,
    pub amount: u64,
}

#[event]
pub struct UserLedgerClosed {
    pub owner: Pubkey,
}

#[account(zero_copy)]
#[repr(C)]
#[derive(Debug)]
pub struct UserPosition {
    pub owner: Pubkey,
    pub market: Pubkey,
    pub yes_shares: u64,
    pub no_shares: u64,
    pub claimed_yes_shares: u64,
    pub claimed_no_shares: u64,
    pub bump: u8,
    pub _reserved: [u8; 7],
}

impl UserPosition {
    pub const INIT_SPACE: usize = core::mem::size_of::<Self>();

    pub fn is_empty_for_close(&self) -> bool {
        self.yes_shares == 0
            && self.no_shares == 0
            && self.claimed_yes_shares == 0
            && self.claimed_no_shares == 0
    }
}

#[event]
pub struct UserPositionInitialized {
    pub owner: Pubkey,
    pub market: Pubkey,
    pub payer: Pubkey,
}

#[event]
pub struct UserPositionClosed {
    pub owner: Pubkey,
    pub market: Pubkey,
}
