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
}
