use anchor_lang::prelude::*;

#[account]
#[derive(InitSpace)]
pub struct GlobalConfig {
    pub admin: Pubkey,
    pub collateral_mint: Pubkey,
    pub global_vault: Pubkey,
    pub relayer_signer: Pubkey,
    pub platform_fee_recipient: Pubkey,
    pub creator_fee_bps: u16,
    pub platform_fee_bps: u16,
    pub vault_authority_bump: u8,
    pub bump: u8,
}
