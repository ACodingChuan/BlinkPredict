use anchor_lang::prelude::*;
use anchor_spl::{
    associated_token::AssociatedToken,
    token::{self, Burn, Mint, MintTo, Token, TokenAccount, Transfer},
};
use pyth_sdk_solana::state::SolanaPriceAccount;

mod error;
mod state;

use error::ErrorCode;
use state::*;

declare_id!("2FoSgViaZXUXL8txXYxc893cUSpPCuvdVZBJ9YDzUKzE");

const DEFAULT_PYTH_STALENESS_SECONDS: u64 = 60;
const TARGET_PRICE_SCALE: i128 = 1_000_000;

#[program]
pub mod predix_program {
    use super::*;

    #[allow(clippy::too_many_arguments)]
    pub fn initialize_market(
        ctx: Context<InitializeMarket>,
        market_id: u64,
        metadata: String,
        expiration_timestamp: i64,
        resolution_mode: ResolutionMode,
        resolution_authority: Pubkey,
        oracle_feed: Pubkey,
        oracle_condition: OracleCondition,
        oracle_target_price: u64,
        oracle_observation_time: i64,
    ) -> Result<()> {
        let now = Clock::get()?.unix_timestamp;
        require!(
            expiration_timestamp > now,
            ErrorCode::InvalidExpirationTimestamp
        );
        if resolution_mode == ResolutionMode::Pyth {
            require!(
                oracle_observation_time <= expiration_timestamp,
                ErrorCode::InvalidExpirationTimestamp
            );
            require!(oracle_feed != Pubkey::default(), ErrorCode::InvalidOracleAccount);
        }

        let market = &mut ctx.accounts.market;
        market.authority = ctx.accounts.admin.key();
        market.metadata_url = metadata;
        market.collateral_vault = ctx.accounts.vault.key();
        market.collateral_mint = ctx.accounts.collateral_mint.key();
        market.yes_mint = ctx.accounts.yes_mint.key();
        market.no_mint = ctx.accounts.no_mint.key();
        market.market_id = market_id;
        market.yes_total = 0;
        market.no_total = 0;
        market.outcome = MarketOutcome::Undecided;
        market.expiration_timestamp = expiration_timestamp;
        market.is_settled = false;
        market.resolution_mode = resolution_mode;
        market.resolution_authority = match resolution_mode {
            ResolutionMode::Creator => resolution_authority,
            ResolutionMode::Pyth => Pubkey::default(),
        };
        market.oracle_feed = match resolution_mode {
            ResolutionMode::Pyth => oracle_feed,
            ResolutionMode::Creator => Pubkey::default(),
        };
        market.oracle_condition = oracle_condition;
        market.oracle_target_price = oracle_target_price;
        market.oracle_observation_time = oracle_observation_time;
        market.resolved_at = 0;
        market.bump = ctx.bumps.market;

        emit!(MarketInitialized {
            market_id,
            market_pda: market.key(),
            authority: market.authority,
            collateral_mint: market.collateral_mint,
            collateral_vault: market.collateral_vault,
            metadata_url: market.metadata_url.clone(),
            yes_mint: market.yes_mint,
            no_mint: market.no_mint,
            expiration_timestamp,
            resolution_mode,
            resolution_authority: market.resolution_authority,
            oracle_feed: market.oracle_feed,
            oracle_condition,
            oracle_target_price,
            oracle_observation_time,
        });

        Ok(())
    }

    pub fn split_token(ctx: Context<SplitToken>, market_id: u64, amount: u64) -> Result<()> {
        let market = &ctx.accounts.market;
        require!(!market.is_settled, ErrorCode::MarketAlreadySettled);
        require!(market.market_id == market_id, ErrorCode::InvalidMarket);
        require!(amount > 0, ErrorCode::InvalidAmount);
        require!(
            Clock::get()?.unix_timestamp < market.expiration_timestamp,
            ErrorCode::MarketExpired
        );
        let market_key = market.key();

        token::transfer(
            CpiContext::new(
                ctx.accounts.token_program.to_account_info(),
                Transfer {
                    from: ctx.accounts.user_collateral.to_account_info(),
                    to: ctx.accounts.collateral_vault.to_account_info(),
                    authority: ctx.accounts.user.to_account_info(),
                },
            ),
            amount,
        )?;

        let market_id_bytes = market.market_id.to_le_bytes();
        let bump_seed = [market.bump];
        let signer_seeds: &[&[u8]] = &[b"market", market_id_bytes.as_ref(), &bump_seed];

        token::mint_to(
            CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                MintTo {
                    mint: ctx.accounts.yes_mint.to_account_info(),
                    to: ctx.accounts.yes_ata.to_account_info(),
                    authority: ctx.accounts.market.to_account_info(),
                },
                &[signer_seeds],
            ),
            amount,
        )?;

        token::mint_to(
            CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                MintTo {
                    mint: ctx.accounts.no_mint.to_account_info(),
                    to: ctx.accounts.no_ata.to_account_info(),
                    authority: ctx.accounts.market.to_account_info(),
                },
                &[signer_seeds],
            ),
            amount,
        )?;

        let market = &mut ctx.accounts.market;
        market.yes_total = market
            .yes_total
            .checked_add(amount)
            .ok_or(ErrorCode::MathOverflow)?;
        market.no_total = market
            .no_total
            .checked_add(amount)
            .ok_or(ErrorCode::MathOverflow)?;

        emit!(TokensSplit {
            market_id,
            market: market_key,
            user: ctx.accounts.user.key(),
            amount,
        });

        Ok(())
    }

    pub fn merge_tokens(ctx: Context<MergeTokens>, market_id: u64, amount: u64) -> Result<()> {
        let market = &ctx.accounts.market;
        require!(!market.is_settled, ErrorCode::MarketAlreadySettled);
        require!(market.market_id == market_id, ErrorCode::InvalidMarket);
        require!(amount > 0, ErrorCode::InvalidAmount);
        let market_key = market.key();

        token::burn(
            CpiContext::new(
                ctx.accounts.token_program.to_account_info(),
                Burn {
                    mint: ctx.accounts.yes_mint.to_account_info(),
                    from: ctx.accounts.yes_ata.to_account_info(),
                    authority: ctx.accounts.user.to_account_info(),
                },
            ),
            amount,
        )?;

        token::burn(
            CpiContext::new(
                ctx.accounts.token_program.to_account_info(),
                Burn {
                    mint: ctx.accounts.no_mint.to_account_info(),
                    from: ctx.accounts.no_ata.to_account_info(),
                    authority: ctx.accounts.user.to_account_info(),
                },
            ),
            amount,
        )?;

        let market_id_bytes = market.market_id.to_le_bytes();
        let bump_seed = [market.bump];
        let signer_seeds: &[&[u8]] = &[b"market", market_id_bytes.as_ref(), &bump_seed];
        token::transfer(
            CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                Transfer {
                    from: ctx.accounts.collateral_vault.to_account_info(),
                    to: ctx.accounts.user_collateral.to_account_info(),
                    authority: ctx.accounts.market.to_account_info(),
                },
                &[signer_seeds],
            ),
            amount,
        )?;

        let market = &mut ctx.accounts.market;
        market.yes_total = market
            .yes_total
            .checked_sub(amount)
            .ok_or(ErrorCode::MathOverflow)?;
        market.no_total = market
            .no_total
            .checked_sub(amount)
            .ok_or(ErrorCode::MathOverflow)?;

        emit!(TokensMerged {
            market_id,
            market: market_key,
            user: ctx.accounts.user.key(),
            amount,
        });

        Ok(())
    }

    pub fn resolve_by_creator(
        ctx: Context<ResolveByCreator>,
        market_id: u64,
        outcome: MarketOutcome,
    ) -> Result<()> {
        let now = Clock::get()?.unix_timestamp;
        let market = &mut ctx.accounts.market;
        require!(market.market_id == market_id, ErrorCode::InvalidMarket);
        require!(!market.is_settled, ErrorCode::MarketAlreadySettled);
        require!(
            market.resolution_mode == ResolutionMode::Creator,
            ErrorCode::InvalidResolutionMode
        );
        require!(outcome != MarketOutcome::Undecided, ErrorCode::InvalidAmount);
        require!(
            ctx.accounts.authority.key() == market.resolution_authority,
            ErrorCode::InvalidMarketAuthority
        );

        market.outcome = outcome;
        market.is_settled = true;
        market.resolved_at = now;

        emit!(MarketResolved {
            market_id,
            market: market.key(),
            resolution_mode: market.resolution_mode,
            outcome,
            resolved_at: now,
        });

        Ok(())
    }

    pub fn resolve_by_pyth(ctx: Context<ResolveByPyth>, market_id: u64) -> Result<()> {
        let clock = Clock::get()?;
        let market = &mut ctx.accounts.market;
        require!(market.market_id == market_id, ErrorCode::InvalidMarket);
        require!(!market.is_settled, ErrorCode::MarketAlreadySettled);
        require!(
            market.resolution_mode == ResolutionMode::Pyth,
            ErrorCode::InvalidResolutionMode
        );
        require!(
            ctx.accounts.oracle_price_feed.key() == market.oracle_feed,
            ErrorCode::InvalidOracleAccount
        );
        require!(
            clock.unix_timestamp >= market.oracle_observation_time,
            ErrorCode::OracleObservationTimeNotReached
        );

        let price_feed = SolanaPriceAccount::account_info_to_feed(&ctx.accounts.oracle_price_feed)
            .map_err(|_| error!(ErrorCode::OraclePriceUnavailable))?;
        let price = price_feed
            .get_price_no_older_than(clock.unix_timestamp, DEFAULT_PYTH_STALENESS_SECONDS)
            .ok_or(error!(ErrorCode::OraclePriceUnavailable))?;
        let observed_price = scale_price(price.price, price.expo)?;
        let outcome = outcome_from_price(
            observed_price,
            market.oracle_target_price,
            market.oracle_condition,
        );

        market.outcome = outcome;
        market.is_settled = true;
        market.resolved_at = clock.unix_timestamp;

        emit!(MarketResolved {
            market_id,
            market: market.key(),
            resolution_mode: market.resolution_mode,
            outcome,
            resolved_at: market.resolved_at,
        });

        Ok(())
    }

    pub fn claim_reward(ctx: Context<ClaimReward>, market_id: u64) -> Result<()> {
        let market = &ctx.accounts.market;
        require!(market.market_id == market_id, ErrorCode::InvalidMarket);
        require!(market.is_settled, ErrorCode::MarketNotSettled);
        let market_key = market.key();

        let payout_amount = match market.outcome {
            MarketOutcome::Yes => ctx.accounts.yes_ata.amount,
            MarketOutcome::No => ctx.accounts.no_ata.amount,
            MarketOutcome::Undecided => 0,
        };
        require!(payout_amount > 0, ErrorCode::InvalidWinningPosition);

        let market_id_bytes = market.market_id.to_le_bytes();
        let bump_seed = [market.bump];
        let signer_seeds: &[&[u8]] = &[b"market", market_id_bytes.as_ref(), &bump_seed];

        let outcome = market.outcome;
        let market = &mut ctx.accounts.market;

        match outcome {
            MarketOutcome::Yes => {
                token::burn(
                    CpiContext::new(
                        ctx.accounts.token_program.to_account_info(),
                        Burn {
                            mint: ctx.accounts.yes_mint.to_account_info(),
                            from: ctx.accounts.yes_ata.to_account_info(),
                            authority: ctx.accounts.user.to_account_info(),
                        },
                    ),
                    payout_amount,
                )?;
                market.yes_total = market
                    .yes_total
                    .checked_sub(payout_amount)
                    .ok_or(ErrorCode::MathOverflow)?;
            }
            MarketOutcome::No => {
                token::burn(
                    CpiContext::new(
                        ctx.accounts.token_program.to_account_info(),
                        Burn {
                            mint: ctx.accounts.no_mint.to_account_info(),
                            from: ctx.accounts.no_ata.to_account_info(),
                            authority: ctx.accounts.user.to_account_info(),
                        },
                    ),
                    payout_amount,
                )?;
                market.no_total = market
                    .no_total
                    .checked_sub(payout_amount)
                    .ok_or(ErrorCode::MathOverflow)?;
            }
            MarketOutcome::Undecided => unreachable!(),
        }

        token::transfer(
            CpiContext::new_with_signer(
                ctx.accounts.token_program.to_account_info(),
                Transfer {
                    from: ctx.accounts.collateral_vault.to_account_info(),
                    to: ctx.accounts.user_collateral.to_account_info(),
                    authority: ctx.accounts.market.to_account_info(),
                },
                &[signer_seeds],
            ),
            payout_amount,
        )?;

        emit!(RewardsClaimed {
            market_id,
            market: market_key,
            user: ctx.accounts.user.key(),
            amount: payout_amount,
            outcome,
        });

        Ok(())
    }
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct InitializeMarket<'info> {
    #[account(
        init,
        payer = admin,
        space = 8 + Market::INIT_SPACE,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub market: Account<'info, Market>,
    #[account(
        init,
        payer = admin,
        token::mint = collateral_mint,
        token::authority = market,
        seeds = [b"collateral_vault", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub vault: Account<'info, TokenAccount>,
    #[account(
        init,
        payer = admin,
        mint::decimals = collateral_mint.decimals,
        mint::authority = market,
        seeds = [b"yes_mint", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub yes_mint: Account<'info, Mint>,
    #[account(
        init,
        payer = admin,
        mint::decimals = collateral_mint.decimals,
        mint::authority = market,
        seeds = [b"no_mint", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub no_mint: Account<'info, Mint>,
    pub collateral_mint: Account<'info, Mint>,
    #[account(mut)]
    pub admin: Signer<'info>,
    pub token_program: Program<'info, Token>,
    pub associated_token_program: Program<'info, AssociatedToken>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct SplitToken<'info> {
    #[account(
        mut,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump = market.bump,
        has_one = collateral_vault,
        has_one = yes_mint,
        has_one = no_mint,
    )]
    pub market: Account<'info, Market>,
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(
        mut,
        constraint = user_collateral.owner == user.key(),
        constraint = user_collateral.mint == market.collateral_mint,
    )]
    pub user_collateral: Account<'info, TokenAccount>,
    #[account(mut)]
    pub collateral_vault: Account<'info, TokenAccount>,
    #[account(mut)]
    pub yes_mint: Account<'info, Mint>,
    #[account(mut)]
    pub no_mint: Account<'info, Mint>,
    #[account(
        init_if_needed,
        payer = user,
        associated_token::mint = yes_mint,
        associated_token::authority = user,
    )]
    pub yes_ata: Account<'info, TokenAccount>,
    #[account(
        init_if_needed,
        payer = user,
        associated_token::mint = no_mint,
        associated_token::authority = user,
    )]
    pub no_ata: Account<'info, TokenAccount>,
    pub token_program: Program<'info, Token>,
    pub associated_token_program: Program<'info, AssociatedToken>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct MergeTokens<'info> {
    #[account(
        mut,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump = market.bump,
        has_one = collateral_vault,
        has_one = yes_mint,
        has_one = no_mint,
    )]
    pub market: Account<'info, Market>,
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(
        mut,
        constraint = user_collateral.owner == user.key(),
        constraint = user_collateral.mint == market.collateral_mint,
    )]
    pub user_collateral: Account<'info, TokenAccount>,
    #[account(mut)]
    pub collateral_vault: Account<'info, TokenAccount>,
    #[account(mut, constraint = yes_ata.owner == user.key(), constraint = yes_ata.mint == yes_mint.key())]
    pub yes_ata: Account<'info, TokenAccount>,
    #[account(mut, constraint = no_ata.owner == user.key(), constraint = no_ata.mint == no_mint.key())]
    pub no_ata: Account<'info, TokenAccount>,
    #[account(mut)]
    pub yes_mint: Account<'info, Mint>,
    #[account(mut)]
    pub no_mint: Account<'info, Mint>,
    pub token_program: Program<'info, Token>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct ResolveByCreator<'info> {
    #[account(
        mut,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump = market.bump,
    )]
    pub market: Account<'info, Market>,
    pub authority: Signer<'info>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct ResolveByPyth<'info> {
    #[account(
        mut,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump = market.bump,
    )]
    pub market: Account<'info, Market>,
    /// CHECK: validated against the oracle feed pubkey stored in market state.
    pub oracle_price_feed: UncheckedAccount<'info>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct ClaimReward<'info> {
    #[account(
        mut,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump = market.bump,
        has_one = collateral_vault,
        has_one = yes_mint,
        has_one = no_mint,
    )]
    pub market: Account<'info, Market>,
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(
        mut,
        constraint = user_collateral.owner == user.key(),
        constraint = user_collateral.mint == market.collateral_mint,
    )]
    pub user_collateral: Account<'info, TokenAccount>,
    #[account(mut)]
    pub collateral_vault: Account<'info, TokenAccount>,
    #[account(mut, constraint = yes_ata.owner == user.key(), constraint = yes_ata.mint == yes_mint.key())]
    pub yes_ata: Account<'info, TokenAccount>,
    #[account(mut, constraint = no_ata.owner == user.key(), constraint = no_ata.mint == no_mint.key())]
    pub no_ata: Account<'info, TokenAccount>,
    #[account(mut)]
    pub yes_mint: Account<'info, Mint>,
    #[account(mut)]
    pub no_mint: Account<'info, Mint>,
    pub token_program: Program<'info, Token>,
}

fn scale_price(price: i64, expo: i32) -> Result<u64> {
    let exponent = i128::from(expo) + TARGET_PRICE_SCALE.ilog10() as i128;
    let price = i128::from(price);
    let scaled = if exponent >= 0 {
        let multiplier = 10_i128.pow(exponent as u32);
        price.checked_mul(multiplier).ok_or(ErrorCode::MathOverflow)?
    } else {
        let divisor = 10_i128.pow((-exponent) as u32);
        price.checked_div(divisor).ok_or(ErrorCode::MathOverflow)?
    };
    require!(scaled >= 0, ErrorCode::OraclePriceUnavailable);
    Ok(scaled as u64)
}

fn outcome_from_price(
    observed_price: u64,
    oracle_target_price: u64,
    oracle_condition: OracleCondition,
) -> MarketOutcome {
    let yes_wins = match oracle_condition {
        OracleCondition::GreaterThan => observed_price > oracle_target_price,
        OracleCondition::GreaterThanOrEqual => observed_price >= oracle_target_price,
        OracleCondition::LessThan => observed_price < oracle_target_price,
        OracleCondition::LessThanOrEqual => observed_price <= oracle_target_price,
    };
    if yes_wins {
        MarketOutcome::Yes
    } else {
        MarketOutcome::No
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn scales_negative_exponent_price_to_micro_units() {
        assert_eq!(scale_price(242_12345678, -8).unwrap(), 242_123456);
    }

    #[test]
    fn compares_oracle_conditions() {
        assert_eq!(
            outcome_from_price(2_500_000, 2_400_000, OracleCondition::GreaterThan),
            MarketOutcome::Yes
        );
        assert_eq!(
            outcome_from_price(2_400_000, 2_400_000, OracleCondition::GreaterThan),
            MarketOutcome::No
        );
        assert_eq!(
            outcome_from_price(2_400_000, 2_400_000, OracleCondition::GreaterThanOrEqual),
            MarketOutcome::Yes
        );
        assert_eq!(
            outcome_from_price(2_300_000, 2_400_000, OracleCondition::LessThan),
            MarketOutcome::Yes
        );
        assert_eq!(
            outcome_from_price(2_400_000, 2_400_000, OracleCondition::LessThanOrEqual),
            MarketOutcome::Yes
        );
    }

    #[test]
    fn target_scale_constant_is_micro_units() {
        assert_eq!(TARGET_PRICE_SCALE, 1_000_000);
    }
}
