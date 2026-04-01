use anchor_lang::prelude::*;

#[error_code]
pub enum ErrorCode {
    #[msg("Invalid market")]
    InvalidMarket,
    #[msg("Invalid expiration timestamp")]
    InvalidExpirationTimestamp,
    #[msg("Market already settled")]
    MarketAlreadySettled,
    #[msg("Market is not settled")]
    MarketNotSettled,
    #[msg("Market has expired")]
    MarketExpired,
    #[msg("Invalid amount")]
    InvalidAmount,
    #[msg("Math overflow")]
    MathOverflow,
    #[msg("Invalid token account")]
    InvalidTokenAccount,
    #[msg("Invalid collateral mint decimals")]
    InvalidCollateralMintDecimals,
    #[msg("Invalid market authority")]
    InvalidMarketAuthority,
    #[msg("Invalid resolution mode for this instruction")]
    InvalidResolutionMode,
    #[msg("Invalid oracle account")]
    InvalidOracleAccount,
    #[msg("Oracle price is unavailable or stale")]
    OraclePriceUnavailable,
    #[msg("Oracle observation time has not been reached")]
    OracleObservationTimeNotReached,
    #[msg("User does not hold winning shares")]
    InvalidWinningPosition,
    #[msg("Unauthorized relayer")]
    UnauthorizedRelayer,
    #[msg("Invalid account owner")]
    InvalidAccountOwner,
    #[msg("Invalid market status")]
    InvalidMarketStatus,
    #[msg("Invalid remaining accounts layout")]
    InvalidRemainingAccountsLayout,
    #[msg("User position is not empty")]
    UserPositionNotEmpty,
    #[msg("Order state is not closable")]
    OrderStateNotClosable,
    #[msg("Invalid user position account")]
    InvalidUserPosition,
    #[msg("Invalid order state account")]
    InvalidOrderState,
    #[msg("Market is not accepting settlement")]
    MarketNotTrading,
    #[msg("Invalid order")]
    InvalidOrder,
    #[msg("Order is expired")]
    OrderExpired,
    #[msg("Order is canceled")]
    OrderCanceled,
    #[msg("Invalid match branch")]
    InvalidMatchBranch,
    #[msg("Insufficient collateral or position")]
    InsufficientCollateral,
    #[msg("Missing matching Ed25519 verification instruction")]
    MissingEd25519Instruction,
    #[msg("Invalid Ed25519 instruction data")]
    InvalidEd25519Instruction,
    #[msg("Market is not closable")]
    MarketNotClosable,
}
