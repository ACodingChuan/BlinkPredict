use anchor_lang::prelude::*;
use anchor_lang::solana_program::{program::invoke_signed, system_instruction};
use anchor_lang::{AccountsExit, Discriminator};
use anchor_spl::token_interface::{
    transfer_checked, Mint, TokenAccount, TokenInterface, TransferChecked,
};
use solana_program::sysvar::instructions::{
    load_current_index_checked, load_instruction_at_checked,
};

mod error;
mod state;
mod utils;

use error::ErrorCode;
use state::*;
use utils::close_account::close_program_account;

declare_id!("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA");

#[program]
pub mod predix_program {
    use super::*;

    pub fn initialize_config(
        ctx: Context<InitializeConfig>,
        relayer_signer: Pubkey,
        platform_fee_recipient: Pubkey,
        creator_fee_bps: u16,
        platform_fee_bps: u16,
    ) -> Result<()> {
        require!(
            creator_fee_bps as u32 + platform_fee_bps as u32 <= 10_000,
            ErrorCode::InvalidAmount
        );

        let config = &mut ctx.accounts.config;
        config.admin = ctx.accounts.admin.key();
        config.collateral_mint = ctx.accounts.collateral_mint.key();
        config.global_vault = ctx.accounts.global_vault.key();
        config.relayer_signer = relayer_signer;
        config.platform_fee_recipient = platform_fee_recipient;
        config.creator_fee_bps = creator_fee_bps;
        config.platform_fee_bps = platform_fee_bps;
        config.vault_authority_bump =
            Pubkey::find_program_address(&[b"global_vault_authority"], &crate::ID).1;
        config.bump = ctx.bumps.config;
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    pub fn create_market(
        ctx: Context<CreateMarket>,
        market_id: u64,
        metadata_cid: [u8; 96],
        close_ts: i64,
        resolve_after_ts: i64,
        claim_deadline_ts: i64,
        resolution_mode: ResolutionMode,
        oracle_feed_id: [u8; 32],
        oracle_condition: OracleCondition,
        oracle_target_price_int: u64,
        oracle_target_expo: i32,
    ) -> Result<()> {
        let now = Clock::get()?.unix_timestamp;
        require!(close_ts > now, ErrorCode::InvalidExpirationTimestamp);
        require!(
            resolve_after_ts >= close_ts,
            ErrorCode::InvalidExpirationTimestamp
        );
        require!(
            claim_deadline_ts > resolve_after_ts,
            ErrorCode::InvalidExpirationTimestamp
        );

        if resolution_mode == ResolutionMode::Pyth {
            require!(
                !is_zero_feed_id(&oracle_feed_id),
                ErrorCode::InvalidOracleAccount
            );
            require!(oracle_target_price_int > 0, ErrorCode::InvalidAmount);
        }

        let config = &ctx.accounts.config;
        let market = &mut ctx.accounts.market;
        market.authority = ctx.accounts.creator.key();
        market.metadata_cid = metadata_cid;
        market.market_id = market_id;
        market.status = MarketStatus::Trading;
        market.outcome = MarketOutcome::Undecided;
        market.close_ts = close_ts;
        market.resolve_after_ts = resolve_after_ts;
        market.claim_deadline_ts = claim_deadline_ts;
        market.resolved_at = 0;
        market.oracle_feed_id = match resolution_mode {
            ResolutionMode::Pyth => oracle_feed_id,
            ResolutionMode::Creator => [0u8; 32],
        };
        market.oracle_condition = oracle_condition;
        market.oracle_target_price_int = oracle_target_price_int;
        market.oracle_target_expo = oracle_target_expo;
        market.creator_fee_bps = config.creator_fee_bps;
        market.platform_fee_bps = config.platform_fee_bps;
        market.creator_unclaimed_fee = 0;
        market.platform_unclaimed_fee = 0;
        market.total_yes_open_interest = 0;
        market.total_no_open_interest = 0;
        market.total_matched_amount = 0;
        market.resolution_mode = resolution_mode;
        market.bump = ctx.bumps.market;

        emit!(MarketCreated {
            market_id,
            market_pda: market.key(),
            authority: market.authority,
            metadata_cid: market.metadata_cid,
            close_ts,
            resolve_after_ts,
            claim_deadline_ts,
            resolution_mode,
            oracle_feed_id: market.oracle_feed_id,
            oracle_condition,
            oracle_target_price_int,
            oracle_target_expo,
        });

        Ok(())
    }

    pub fn init_user_position(ctx: Context<InitUserPosition>) -> Result<()> {
        require_keys_eq!(
            ctx.accounts.payer.key(),
            ctx.accounts.config.relayer_signer,
            ErrorCode::UnauthorizedRelayer
        );

        let position = &mut ctx.accounts.user_position.load_init()?;
        position.owner = ctx.accounts.user.key();
        position.market = ctx.accounts.market.key();
        position.yes_shares = 0;
        position.no_shares = 0;
        position.claimed_yes_shares = 0;
        position.claimed_no_shares = 0;
        position.bump = ctx.bumps.user_position;
        position._reserved = [0; 7];

        emit!(UserPositionInitialized {
            owner: ctx.accounts.user.key(),
            market: ctx.accounts.market.key(),
            payer: ctx.accounts.payer.key(),
        });

        Ok(())
    }

    pub fn init_user_ledger(ctx: Context<InitUserLedger>) -> Result<()> {
        let ledger = &mut ctx.accounts.user_ledger.load_init()?;
        ledger.owner = ctx.accounts.user.key();
        ledger.available_usdc = 0;
        ledger.cancel_all_before_ts = 0;
        ledger.bump = ctx.bumps.user_ledger;
        ledger._reserved = [0; 7];

        emit!(UserLedgerInitialized {
            owner: ctx.accounts.user.key(),
        });
        Ok(())
    }

    pub fn deposit(ctx: Context<Deposit>, amount: u64) -> Result<()> {
        require!(amount > 0, ErrorCode::InvalidAmount);
        require_keys_eq!(
            ctx.accounts.collateral_mint.key(),
            ctx.accounts.config.collateral_mint,
            ErrorCode::InvalidTokenAccount
        );
        require_keys_eq!(
            ctx.accounts.global_vault.key(),
            ctx.accounts.config.global_vault,
            ErrorCode::InvalidTokenAccount
        );
        require_keys_eq!(
            ctx.accounts.user_token_account.owner,
            ctx.accounts.user.key(),
            ErrorCode::InvalidTokenAccount
        );
        require_keys_eq!(
            ctx.accounts.user_token_account.mint,
            ctx.accounts.collateral_mint.key(),
            ErrorCode::InvalidTokenAccount
        );
        require_keys_eq!(
            ctx.accounts.global_vault.owner,
            ctx.accounts.global_vault_authority.key(),
            ErrorCode::InvalidTokenAccount
        );
        require_keys_eq!(
            ctx.accounts.global_vault.mint,
            ctx.accounts.collateral_mint.key(),
            ErrorCode::InvalidTokenAccount
        );

        let multiplier = ledger_unit_multiplier(ctx.accounts.collateral_mint.decimals)?;
        let token_amount = amount
            .checked_mul(multiplier)
            .ok_or(ErrorCode::MathOverflow)?;

        let cpi_accounts = TransferChecked {
            from: ctx.accounts.user_token_account.to_account_info(),
            mint: ctx.accounts.collateral_mint.to_account_info(),
            to: ctx.accounts.global_vault.to_account_info(),
            authority: ctx.accounts.user.to_account_info(),
        };
        let cpi_ctx = CpiContext::new(ctx.accounts.token_program.to_account_info(), cpi_accounts);
        transfer_checked(cpi_ctx, token_amount, ctx.accounts.collateral_mint.decimals)?;

        let user_key = ctx.accounts.user.key();
        if let Ok(mut ledger) = ctx.accounts.user_ledger.load_init() {
            ledger.owner = user_key;
            ledger.available_usdc = amount;
            ledger.cancel_all_before_ts = 0;
            ledger.bump = ctx.bumps.user_ledger;
            ledger._reserved = [0; 7];
        } else {
            let mut ledger = ctx.accounts.user_ledger.load_mut()?;
            require_keys_eq!(ledger.owner, user_key, ErrorCode::InvalidAccountOwner);
            ledger.available_usdc = ledger
                .available_usdc
                .checked_add(amount)
                .ok_or(ErrorCode::MathOverflow)?;
        }

        emit!(DepositSettled {
            owner: ctx.accounts.user.key(),
            amount,
        });
        Ok(())
    }

    pub fn settle_match_batch<'info>(
        ctx: Context<'_, '_, 'info, 'info, SettleMatchBatch<'info>>,
        args: SettleMatchBatchArgs,
    ) -> Result<()> {
        require_keys_eq!(
            ctx.accounts.relayer.key(),
            ctx.accounts.config.relayer_signer,
            ErrorCode::UnauthorizedRelayer
        );

        let now = Clock::get()?.unix_timestamp;
        require!(
            ctx.accounts.market.status == MarketStatus::Trading,
            ErrorCode::MarketNotTrading
        );
        require!(now < ctx.accounts.market.close_ts, ErrorCode::MarketExpired);

        let unique_users = collect_unique_users(&args.orders)?;
        let unique_user_count = unique_users.len();
        let order_count = args.orders.len();
        let expected_accounts = unique_user_count
            .checked_mul(2)
            .and_then(|v| v.checked_add(order_count))
            .ok_or(ErrorCode::MathOverflow)?;
        require!(
            ctx.remaining_accounts.len() == expected_accounts,
            ErrorCode::InvalidRemainingAccountsLayout
        );

        for order in &args.orders {
            require!(order.version == 1, ErrorCode::InvalidOrder);
            require!(order.chain_id > 0, ErrorCode::InvalidOrder);
            require_keys_eq!(order.program_id, crate::ID, ErrorCode::InvalidOrder);
            require_keys_eq!(
                order.market,
                ctx.accounts.market.key(),
                ErrorCode::InvalidMarket
            );
            require!(order.total_amount > 0, ErrorCode::InvalidAmount);
            require!(
                order.limit_price > 0 && order.limit_price < 100,
                ErrorCode::InvalidAmount
            );
            require!(order.expiry_ts >= now, ErrorCode::OrderExpired);
        }

        let (ledger_accounts, position_accounts, order_state_accounts) =
            split_remaining_accounts(ctx.remaining_accounts, unique_user_count, order_count)?;
        validate_user_accounts(
            ctx.accounts.market.key(),
            &unique_users,
            ledger_accounts,
            position_accounts,
        )?;

        let mut market_yes_oi = 0i128;
        let mut market_no_oi = 0i128;
        let mut market_volume = 0u64;
        let mut creator_fee_delta = 0u64;
        let mut platform_fee_delta = 0u64;

        for fill in &args.fills {
            require!(fill.fill_amount > 0, ErrorCode::InvalidAmount);
            require!(
                (fill.maker_idx as usize) < order_count,
                ErrorCode::InvalidRemainingAccountsLayout
            );
            require!(
                (fill.taker_idx as usize) < order_count,
                ErrorCode::InvalidRemainingAccountsLayout
            );
            require!(
                fill.fill_price > 0 && fill.fill_price < 100,
                ErrorCode::InvalidAmount
            );

            let maker = &args.orders[fill.maker_idx as usize];
            let taker = &args.orders[fill.taker_idx as usize];
            let maker_user_idx = find_user_index(&unique_users, &maker.user)?;
            let taker_user_idx = find_user_index(&unique_users, &taker.user)?;

            let maker_ledger_loader =
                AccountLoader::<UserLedger>::try_from(&ledger_accounts[maker_user_idx])?;
            let taker_ledger_loader =
                AccountLoader::<UserLedger>::try_from(&ledger_accounts[taker_user_idx])?;
            let maker_position_loader =
                AccountLoader::<UserPosition>::try_from(&position_accounts[maker_user_idx])?;
            let taker_position_loader =
                AccountLoader::<UserPosition>::try_from(&position_accounts[taker_user_idx])?;

            let maker_order_state_loader = load_or_init_order_state(
                &ctx.accounts.relayer,
                &ctx.accounts.system_program,
                &ctx.accounts.market,
                &order_state_accounts[fill.maker_idx as usize],
                maker,
                &ctx.accounts.instruction_sysvar,
            )?;
            let taker_order_state_loader = load_or_init_order_state(
                &ctx.accounts.relayer,
                &ctx.accounts.system_program,
                &ctx.accounts.market,
                &order_state_accounts[fill.taker_idx as usize],
                taker,
                &ctx.accounts.instruction_sysvar,
            )?;

            {
                let maker_ledger = maker_ledger_loader.load()?;
                let maker_order_state = maker_order_state_loader.load()?;
                validate_order_state(
                    maker,
                    &maker_order_state,
                    &maker_ledger,
                    now,
                    fill.fill_amount,
                    fill.fill_price,
                )?;
            }
            {
                let taker_ledger = taker_ledger_loader.load()?;
                let taker_order_state = taker_order_state_loader.load()?;
                validate_order_state(
                    taker,
                    &taker_order_state,
                    &taker_ledger,
                    now,
                    fill.fill_amount,
                    fill.fill_price,
                )?;
            }

            let branch = classify_branch(maker, taker)?;
            let trade_result = apply_fill(
                branch,
                fill,
                maker,
                taker,
                &ctx.accounts.market,
                &maker_ledger_loader,
                &taker_ledger_loader,
                &maker_position_loader,
                &taker_position_loader,
            )?;

            {
                let maker_progress =
                    order_progress_amount(maker, fill.fill_amount, fill.fill_price)?;
                let mut maker_order_state = maker_order_state_loader.load_mut()?;
                maker_order_state.filled_amount = maker_order_state
                    .filled_amount
                    .checked_add(maker_progress)
                    .ok_or(ErrorCode::MathOverflow)?;
            }
            {
                let taker_progress =
                    order_progress_amount(taker, fill.fill_amount, fill.fill_price)?;
                let mut taker_order_state = taker_order_state_loader.load_mut()?;
                taker_order_state.filled_amount = taker_order_state
                    .filled_amount
                    .checked_add(taker_progress)
                    .ok_or(ErrorCode::MathOverflow)?;
            }

            market_yes_oi = market_yes_oi
                .checked_add(trade_result.yes_open_interest_delta)
                .ok_or(ErrorCode::MathOverflow)?;
            market_no_oi = market_no_oi
                .checked_add(trade_result.no_open_interest_delta)
                .ok_or(ErrorCode::MathOverflow)?;
            market_volume = market_volume
                .checked_add(fill.fill_amount)
                .ok_or(ErrorCode::MathOverflow)?;
            creator_fee_delta = creator_fee_delta
                .checked_add(trade_result.creator_fee)
                .ok_or(ErrorCode::MathOverflow)?;
            platform_fee_delta = platform_fee_delta
                .checked_add(trade_result.platform_fee)
                .ok_or(ErrorCode::MathOverflow)?;

            emit!(MatchSettled {
                market: ctx.accounts.market.key(),
                branch,
                maker: maker.user,
                taker: taker.user,
                fill_amount: fill.fill_amount,
                fill_price: fill.fill_price,
            });
        }

        apply_market_delta(
            &mut ctx.accounts.market,
            market_yes_oi,
            market_no_oi,
            market_volume,
            creator_fee_delta,
            platform_fee_delta,
        )?;

        emit!(SettlementBatchAccepted {
            market: ctx.accounts.market.key(),
            order_count: args.orders.len() as u16,
            fill_count: args.fills.len() as u16,
        });

        Ok(())
    }

    pub fn close_empty_user_position(ctx: Context<CloseEmptyUserPosition>) -> Result<()> {
        let position = ctx.accounts.user_position.load()?;
        require_keys_eq!(
            position.owner,
            ctx.accounts.owner.key(),
            ErrorCode::InvalidUserPosition
        );
        require_keys_eq!(
            ctx.accounts.receiver.key(),
            ctx.accounts.owner.key(),
            ErrorCode::InvalidAccountOwner
        );
        require_keys_eq!(
            position.market,
            ctx.accounts.market.key(),
            ErrorCode::InvalidMarket
        );
        require!(
            position.is_empty_for_close(),
            ErrorCode::UserPositionNotEmpty
        );
        let owner = position.owner;
        let market = position.market;
        drop(position);

        close_program_account(
            &ctx.accounts.user_position.to_account_info(),
            &ctx.accounts.receiver.to_account_info(),
        )?;

        emit!(UserPositionClosed { owner, market });
        Ok(())
    }

    pub fn close_empty_order_state(ctx: Context<CloseEmptyOrderState>) -> Result<()> {
        let order_state = ctx.accounts.order_state.load()?;
        require_keys_eq!(
            order_state.owner,
            ctx.accounts.owner.key(),
            ErrorCode::InvalidOrderState
        );
        require_keys_eq!(
            ctx.accounts.receiver.key(),
            ctx.accounts.owner.key(),
            ErrorCode::InvalidAccountOwner
        );
        require!(
            order_state.is_empty_for_close(),
            ErrorCode::OrderStateNotClosable
        );
        let owner = order_state.owner;
        let nonce = order_state.nonce;
        drop(order_state);

        let expected = Pubkey::find_program_address(
            &[
                b"order",
                ctx.accounts.owner.key().as_ref(),
                ctx.accounts.market.key().as_ref(),
                &nonce.to_le_bytes(),
            ],
            &crate::ID,
        )
        .0;
        require_keys_eq!(
            expected,
            ctx.accounts.order_state.key(),
            ErrorCode::InvalidOrderState
        );

        close_program_account(
            &ctx.accounts.order_state.to_account_info(),
            &ctx.accounts.receiver.to_account_info(),
        )?;

        emit!(OrderStateClosed {
            owner,
            market: ctx.accounts.market.key(),
            nonce,
        });
        Ok(())
    }

    pub fn close_user_ledger(ctx: Context<CloseUserLedger>) -> Result<()> {
        let ledger = ctx.accounts.user_ledger.load()?;
        require_keys_eq!(
            ledger.owner,
            ctx.accounts.owner.key(),
            ErrorCode::InvalidAccountOwner
        );
        require_keys_eq!(
            ctx.accounts.receiver.key(),
            ctx.accounts.owner.key(),
            ErrorCode::InvalidAccountOwner
        );
        require!(
            ledger.available_usdc == 0,
            ErrorCode::InsufficientCollateral
        );
        let owner = ledger.owner;
        drop(ledger);

        close_program_account(
            &ctx.accounts.user_ledger.to_account_info(),
            &ctx.accounts.receiver.to_account_info(),
        )?;

        emit!(UserLedgerClosed { owner });
        Ok(())
    }

    pub fn close_resolved_market(ctx: Context<CloseResolvedMarket>) -> Result<()> {
        require_keys_eq!(
            ctx.accounts.admin.key(),
            ctx.accounts.config.admin,
            ErrorCode::InvalidAccountOwner
        );
        let now = Clock::get()?.unix_timestamp;
        require!(
            ctx.accounts.market.status == MarketStatus::Resolved,
            ErrorCode::MarketNotClosable
        );
        require!(
            now >= ctx.accounts.market.claim_deadline_ts,
            ErrorCode::MarketNotClosable
        );
        require!(
            ctx.accounts.market.creator_unclaimed_fee == 0
                && ctx.accounts.market.platform_unclaimed_fee == 0
                && ctx.accounts.market.total_yes_open_interest == 0
                && ctx.accounts.market.total_no_open_interest == 0,
            ErrorCode::MarketNotClosable
        );

        let market = ctx.accounts.market.key();
        close_program_account(
            &ctx.accounts.market.to_account_info(),
            &ctx.accounts.receiver.to_account_info(),
        )?;
        emit!(MarketClosed {
            market,
            closed_at: now,
        });
        Ok(())
    }
}

#[derive(Accounts)]
pub struct InitializeConfig<'info> {
    #[account(mut)]
    pub admin: Signer<'info>,
    #[account(
        init,
        payer = admin,
        space = 8 + GlobalConfig::INIT_SPACE,
        seeds = [b"config"],
        bump
    )]
    pub config: Account<'info, GlobalConfig>,
    /// CHECK: validated off-chain during deployment/configuration.
    pub collateral_mint: UncheckedAccount<'info>,
    /// CHECK: validated off-chain during deployment/configuration.
    pub global_vault: UncheckedAccount<'info>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct CreateMarket<'info> {
    #[account(
        init,
        payer = creator,
        space = 8 + MarketState::INIT_SPACE,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub market: Account<'info, MarketState>,
    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,
    #[account(mut)]
    pub creator: Signer<'info>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct InitUserPosition<'info> {
    #[account(mut)]
    pub payer: Signer<'info>,
    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,
    /// CHECK: only used as a PDA seed and owner field.
    pub user: UncheckedAccount<'info>,
    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
    #[account(
        init,
        payer = payer,
        space = 8 + UserPosition::INIT_SPACE,
        seeds = [b"position", user.key().as_ref(), market.key().as_ref()],
        bump
    )]
    pub user_position: AccountLoader<'info, UserPosition>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct InitUserLedger<'info> {
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(
        init,
        payer = user,
        space = 8 + UserLedger::INIT_SPACE,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump
    )]
    pub user_ledger: AccountLoader<'info, UserLedger>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct Deposit<'info> {
    #[account(mut)]
    pub user: Signer<'info>,
    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,
    #[account(
        init_if_needed,
        payer = user,
        space = 8 + UserLedger::INIT_SPACE,
        seeds = [b"user_ledger", user.key().as_ref()],
        bump
    )]
    pub user_ledger: AccountLoader<'info, UserLedger>,
    #[account(mut)]
    pub user_token_account: InterfaceAccount<'info, TokenAccount>,
    #[account(mut)]
    pub global_vault: InterfaceAccount<'info, TokenAccount>,
    /// CHECK: PDA signer only.
    #[account(
        seeds = [b"global_vault_authority"],
        bump = config.vault_authority_bump,
    )]
    pub global_vault_authority: UncheckedAccount<'info>,
    pub collateral_mint: InterfaceAccount<'info, Mint>,
    pub token_program: Interface<'info, TokenInterface>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
#[instruction(args: SettleMatchBatchArgs)]
pub struct SettleMatchBatch<'info> {
    #[account(mut)]
    pub relayer: Signer<'info>,
    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,
    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
    /// CHECK: instruction sysvar for ed25519 introspection.
    #[account(address = anchor_lang::solana_program::sysvar::instructions::ID)]
    pub instruction_sysvar: UncheckedAccount<'info>,
    pub system_program: Program<'info, System>,
}

#[derive(Accounts)]
pub struct CloseEmptyUserPosition<'info> {
    #[account(mut)]
    pub owner: Signer<'info>,
    #[account(mut)]
    pub receiver: SystemAccount<'info>,
    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
    #[account(mut)]
    pub user_position: AccountLoader<'info, UserPosition>,
}

#[derive(Accounts)]
pub struct CloseEmptyOrderState<'info> {
    #[account(mut)]
    pub owner: Signer<'info>,
    #[account(mut)]
    pub receiver: SystemAccount<'info>,
    #[account(
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
    #[account(mut)]
    pub order_state: AccountLoader<'info, OrderState>,
}

#[derive(Accounts)]
pub struct CloseUserLedger<'info> {
    #[account(mut)]
    pub owner: Signer<'info>,
    #[account(mut)]
    pub receiver: SystemAccount<'info>,
    #[account(mut)]
    pub user_ledger: AccountLoader<'info, UserLedger>,
}

#[derive(Accounts)]
pub struct CloseResolvedMarket<'info> {
    #[account(mut)]
    pub admin: Signer<'info>,
    #[account(mut)]
    pub receiver: SystemAccount<'info>,
    #[account(seeds = [b"config"], bump = config.bump)]
    pub config: Account<'info, GlobalConfig>,
    #[account(
        mut,
        seeds = [b"market", &market.market_id.to_le_bytes()],
        bump = market.bump,
    )]
    pub market: Account<'info, MarketState>,
}

fn is_zero_feed_id(value: &[u8; 32]) -> bool {
    value.iter().all(|item| *item == 0)
}

fn collect_unique_users(orders: &[OrderIntentV1]) -> Result<Vec<Pubkey>> {
    let mut users = Vec::with_capacity(orders.len());
    for order in orders {
        if !users.iter().any(|existing| existing == &order.user) {
            users.push(order.user);
        }
    }
    Ok(users)
}

fn split_remaining_accounts<'info>(
    accounts: &'info [AccountInfo<'info>],
    user_count: usize,
    order_count: usize,
) -> Result<(
    &'info [AccountInfo<'info>],
    &'info [AccountInfo<'info>],
    &'info [AccountInfo<'info>],
)> {
    let min_expected = user_count
        .checked_mul(2)
        .and_then(|v| v.checked_add(order_count))
        .ok_or(ErrorCode::MathOverflow)?;
    require!(
        accounts.len() >= min_expected,
        ErrorCode::InvalidRemainingAccountsLayout
    );
    let (ledgers, rest) = accounts.split_at(user_count);
    let (positions, order_states) = rest.split_at(user_count);
    require!(
        order_states.len() == order_count,
        ErrorCode::InvalidRemainingAccountsLayout
    );
    Ok((ledgers, positions, order_states))
}

fn find_user_index(users: &[Pubkey], user: &Pubkey) -> Result<usize> {
    users
        .iter()
        .position(|candidate| candidate == user)
        .ok_or_else(|| error!(ErrorCode::InvalidRemainingAccountsLayout))
}

fn ledger_unit_multiplier(decimals: u8) -> Result<u64> {
    require!(decimals >= 2, ErrorCode::InvalidCollateralMintDecimals);
    let exponent = decimals - 2;
    let mut multiplier = 1u64;
    for _ in 0..exponent {
        multiplier = multiplier.checked_mul(10).ok_or(ErrorCode::MathOverflow)?;
    }
    Ok(multiplier)
}

fn validate_user_accounts<'info>(
    market_key: Pubkey,
    users: &[Pubkey],
    ledgers: &'info [AccountInfo<'info>],
    positions: &'info [AccountInfo<'info>],
) -> Result<()> {
    for (idx, user) in users.iter().enumerate() {
        let ledger_pda =
            Pubkey::find_program_address(&[b"user_ledger", user.as_ref()], &crate::ID).0;
        require_keys_eq!(
            ledger_pda,
            *ledgers[idx].key,
            ErrorCode::InvalidRemainingAccountsLayout
        );

        let position_pda = Pubkey::find_program_address(
            &[b"position", user.as_ref(), market_key.as_ref()],
            &crate::ID,
        )
        .0;
        require_keys_eq!(
            position_pda,
            *positions[idx].key,
            ErrorCode::InvalidRemainingAccountsLayout
        );

        let ledger = AccountLoader::<UserLedger>::try_from(&ledgers[idx])?;
        let ledger_data = ledger.load()?;
        require_keys_eq!(ledger_data.owner, *user, ErrorCode::InvalidAccountOwner);
        drop(ledger_data);

        let position = AccountLoader::<UserPosition>::try_from(&positions[idx])?;
        let position_data = position.load()?;
        require_keys_eq!(position_data.owner, *user, ErrorCode::InvalidUserPosition);
        require_keys_eq!(position_data.market, market_key, ErrorCode::InvalidMarket);
    }
    Ok(())
}

fn load_or_init_order_state<'info>(
    payer: &Signer<'info>,
    system_program: &Program<'info, System>,
    market: &Account<'info, MarketState>,
    order_state_ai: &'info AccountInfo<'info>,
    order: &OrderIntentV1,
    instruction_sysvar: &UncheckedAccount<'info>,
) -> Result<anchor_lang::accounts::account_loader::AccountLoader<'info, OrderState>> {
    let (expected_pda, bump) = Pubkey::find_program_address(
        &[
            b"order",
            order.user.as_ref(),
            market.key().as_ref(),
            &order.nonce.to_le_bytes(),
        ],
        &crate::ID,
    );
    require_keys_eq!(
        expected_pda,
        *order_state_ai.key,
        ErrorCode::InvalidOrderState
    );

    if order_state_ai.owner == &anchor_lang::system_program::ID {
        verify_ed25519_instruction(order, instruction_sysvar)?;
        let rent = Rent::get()?;
        let space = 8 + OrderState::INIT_SPACE;
        let lamports = rent.minimum_balance(space);
        let market_key = market.key();
        let nonce_bytes = order.nonce.to_le_bytes();
        let bump_seed = [bump];
        let seeds: &[&[u8]] = &[
            b"order",
            order.user.as_ref(),
            market_key.as_ref(),
            &nonce_bytes,
            &bump_seed,
        ];
        invoke_signed(
            &system_instruction::create_account(
                &payer.key(),
                order_state_ai.key,
                lamports,
                space as u64,
                &crate::ID,
            ),
            &[
                payer.to_account_info(),
                order_state_ai.clone(),
                system_program.to_account_info(),
            ],
            &[seeds],
        )?;

        let loader = AccountLoader::<OrderState>::try_from_unchecked(&crate::ID, order_state_ai)?;
        {
            let mut state = loader.load_init()?;
            state.owner = order.user;
            state.nonce = order.nonce;
            state.order_hash = order_hash(order)?;
            state.total_amount = order.total_amount;
            state.filled_amount = 0;
            state.canceled = 0;
            state.bump = bump;
            state._reserved = [0; 6];
        }
        loader.exit(&crate::ID)?;
        return Ok(loader);
    }

    AccountLoader::<OrderState>::try_from(order_state_ai)
}

fn verify_ed25519_instruction(
    order: &OrderIntentV1,
    instruction_sysvar: &UncheckedAccount<'_>,
) -> Result<()> {
    let current_idx = load_current_index_checked(&instruction_sysvar.to_account_info())?;
    let expected_pubkey = order.user.to_bytes();
    let expected_message = order_signed_message_bytes(order)?;

    for idx in 0..current_idx {
        let instruction =
            load_instruction_at_checked(idx as usize, &instruction_sysvar.to_account_info())?;
        if !solana_program::ed25519_program::check_id(&instruction.program_id) {
            continue;
        }
        if ed25519_instruction_matches(&instruction.data, &expected_pubkey, &expected_message)? {
            return Ok(());
        }
    }

    Err(error!(ErrorCode::MissingEd25519Instruction))
}

fn ed25519_instruction_matches(
    data: &[u8],
    expected_pubkey: &[u8; 32],
    expected_message: &[u8],
) -> Result<bool> {
    if data.len() < 2 {
        return Err(error!(ErrorCode::InvalidEd25519Instruction));
    }
    let signature_count = data[0] as usize;
    let offsets_len = signature_count
        .checked_mul(14)
        .ok_or(ErrorCode::MathOverflow)?;
    if data.len() < 2 + offsets_len {
        return Err(error!(ErrorCode::InvalidEd25519Instruction));
    }

    for idx in 0..signature_count {
        let base = 2 + idx * 14;
        let signature_offset = read_u16(data, base)? as usize;
        let signature_instruction_index = read_u16(data, base + 2)?;
        let public_key_offset = read_u16(data, base + 4)? as usize;
        let public_key_instruction_index = read_u16(data, base + 6)?;
        let message_offset = read_u16(data, base + 8)? as usize;
        let message_size = read_u16(data, base + 10)? as usize;
        let message_instruction_index = read_u16(data, base + 12)?;

        if signature_instruction_index != u16::MAX
            || public_key_instruction_index != u16::MAX
            || message_instruction_index != u16::MAX
        {
            continue;
        }

        let _signature = read_slice(data, signature_offset, 64)?;
        let pubkey = read_slice(data, public_key_offset, 32)?;
        let message = read_slice(data, message_offset, message_size)?;
        if pubkey == expected_pubkey && message == expected_message {
            return Ok(true);
        }
    }

    Ok(false)
}

fn read_u16(data: &[u8], offset: usize) -> Result<u16> {
    let bytes = read_slice(data, offset, 2)?;
    Ok(u16::from_le_bytes([bytes[0], bytes[1]]))
}

fn read_slice(data: &[u8], offset: usize, len: usize) -> Result<&[u8]> {
    let end = offset.checked_add(len).ok_or(ErrorCode::MathOverflow)?;
    if end > data.len() {
        return Err(error!(ErrorCode::InvalidEd25519Instruction));
    }
    Ok(&data[offset..end])
}

fn validate_order_state(
    order: &OrderIntentV1,
    order_state: &OrderState,
    ledger: &UserLedger,
    now: i64,
    fill_amount: u64,
    fill_price: u64,
) -> Result<()> {
    let progress_amount = order_progress_amount(order, fill_amount, fill_price)?;
    require_keys_eq!(order_state.owner, order.user, ErrorCode::InvalidOrderState);
    require_keys_eq!(ledger.owner, order.user, ErrorCode::InvalidAccountOwner);
    require!(
        order_state.nonce == order.nonce,
        ErrorCode::InvalidOrderState
    );
    require!(order_state.canceled == 0, ErrorCode::OrderCanceled);
    require!(order.expiry_ts >= now, ErrorCode::OrderExpired);
    require!(
        extract_order_timestamp_ms(order.nonce)? >= ledger.cancel_all_before_ts,
        ErrorCode::OrderCanceled
    );
    require!(
        order_state.total_amount == order.total_amount,
        ErrorCode::InvalidOrderState
    );
    require!(
        order_state.order_hash == order_hash(order)?,
        ErrorCode::InvalidOrderState
    );
    require!(
        order_state
            .filled_amount
            .checked_add(progress_amount)
            .ok_or(ErrorCode::MathOverflow)?
            <= order_state.total_amount,
        ErrorCode::InvalidOrderState
    );
    validate_fill_price_for_order(order, fill_price)?;
    Ok(())
}

fn validate_fill_price_for_order(order: &OrderIntentV1, fill_price: u64) -> Result<()> {
    require!(fill_price > 0 && fill_price < 100, ErrorCode::InvalidAmount);
    let limit_tick = order.limit_price;
    let effective_tick = match order.outcome {
        Outcome::Yes => fill_price,
        Outcome::No => 100u64
            .checked_sub(fill_price)
            .ok_or(ErrorCode::MathOverflow)?,
    };
    match order.side {
        Side::Buy => require!(effective_tick <= limit_tick, ErrorCode::InvalidOrder),
        Side::Sell => require!(effective_tick >= limit_tick, ErrorCode::InvalidOrder),
    }
    Ok(())
}

fn extract_order_timestamp_ms(nonce: u64) -> Result<i64> {
    let millis = nonce >> 22;
    i64::try_from(millis).map_err(|_| error!(ErrorCode::MathOverflow))
}

fn order_progress_amount(order: &OrderIntentV1, fill_amount: u64, fill_price: u64) -> Result<u64> {
    if order.order_type == OrderType::Market && order.side == Side::Buy {
        return trade_cash_for_order(order, fill_amount, fill_price);
    }
    Ok(fill_amount)
}

fn classify_branch(maker: &OrderIntentV1, taker: &OrderIntentV1) -> Result<SettleBranch> {
    match (maker.side, taker.side) {
        (Side::Buy, Side::Buy) => {
            require!(
                maker.outcome != taker.outcome,
                ErrorCode::InvalidMatchBranch
            );
            Ok(SettleBranch::MatchAndMint)
        }
        (Side::Sell, Side::Sell) => {
            require!(
                maker.outcome != taker.outcome,
                ErrorCode::InvalidMatchBranch
            );
            Ok(SettleBranch::MergeAndBurn)
        }
        _ => {
            require!(
                maker.outcome == taker.outcome,
                ErrorCode::InvalidMatchBranch
            );
            Ok(SettleBranch::Transfer)
        }
    }
}

struct TradeResult {
    creator_fee: u64,
    platform_fee: u64,
    yes_open_interest_delta: i128,
    no_open_interest_delta: i128,
}

fn apply_fill<'info>(
    branch: SettleBranch,
    fill: &FillIndexPair,
    maker: &OrderIntentV1,
    taker: &OrderIntentV1,
    market: &Account<'info, MarketState>,
    maker_ledger_loader: &AccountLoader<'info, UserLedger>,
    taker_ledger_loader: &AccountLoader<'info, UserLedger>,
    maker_position_loader: &AccountLoader<'info, UserPosition>,
    taker_position_loader: &AccountLoader<'info, UserPosition>,
) -> Result<TradeResult> {
    let maker_cash = trade_cash_for_order(maker, fill.fill_amount, fill.fill_price)?;
    let taker_cash = trade_cash_for_order(taker, fill.fill_amount, fill.fill_price)?;
    let taker_creator_fee = ceil_mul_div(taker_cash, market.creator_fee_bps as u64, 10_000)?;
    let taker_platform_fee = ceil_mul_div(taker_cash, market.platform_fee_bps as u64, 10_000)?;
    let taker_total_fee = taker_creator_fee
        .checked_add(taker_platform_fee)
        .ok_or(ErrorCode::MathOverflow)?;

    match branch {
        SettleBranch::MatchAndMint => {
            debit_buyer(maker_ledger_loader, maker_cash, false, 0)?;
            debit_buyer(taker_ledger_loader, taker_cash, true, taker_total_fee)?;
            credit_position(
                maker_position_loader,
                maker.outcome,
                fill.fill_amount as i128,
            )?;
            credit_position(
                taker_position_loader,
                taker.outcome,
                fill.fill_amount as i128,
            )?;
            Ok(TradeResult {
                creator_fee: taker_creator_fee,
                platform_fee: taker_platform_fee,
                yes_open_interest_delta: 1i128
                    .checked_mul(fill.fill_amount as i128)
                    .ok_or(ErrorCode::MathOverflow)?,
                no_open_interest_delta: 1i128
                    .checked_mul(fill.fill_amount as i128)
                    .ok_or(ErrorCode::MathOverflow)?,
            })
        }
        SettleBranch::Transfer => {
            let maker_is_buy = maker.side == Side::Buy;
            if maker_is_buy {
                debit_buyer(maker_ledger_loader, maker_cash, false, 0)?;
                credit_seller(taker_ledger_loader, taker_cash, true, taker_total_fee)?;
                credit_position(
                    maker_position_loader,
                    maker.outcome,
                    fill.fill_amount as i128,
                )?;
                credit_position(
                    taker_position_loader,
                    taker.outcome,
                    -(fill.fill_amount as i128),
                )?;
            } else {
                credit_seller(maker_ledger_loader, maker_cash, false, 0)?;
                debit_buyer(taker_ledger_loader, taker_cash, true, taker_total_fee)?;
                credit_position(
                    maker_position_loader,
                    maker.outcome,
                    -(fill.fill_amount as i128),
                )?;
                credit_position(
                    taker_position_loader,
                    taker.outcome,
                    fill.fill_amount as i128,
                )?;
            }
            Ok(TradeResult {
                creator_fee: taker_creator_fee,
                platform_fee: taker_platform_fee,
                yes_open_interest_delta: 0,
                no_open_interest_delta: 0,
            })
        }
        SettleBranch::MergeAndBurn => {
            credit_seller(maker_ledger_loader, maker_cash, false, 0)?;
            credit_seller(taker_ledger_loader, taker_cash, true, taker_total_fee)?;
            credit_position(
                maker_position_loader,
                maker.outcome,
                -(fill.fill_amount as i128),
            )?;
            credit_position(
                taker_position_loader,
                taker.outcome,
                -(fill.fill_amount as i128),
            )?;
            Ok(TradeResult {
                creator_fee: taker_creator_fee,
                platform_fee: taker_platform_fee,
                yes_open_interest_delta: -(fill.fill_amount as i128),
                no_open_interest_delta: -(fill.fill_amount as i128),
            })
        }
    }
}

fn debit_buyer<'info>(
    ledger_loader: &AccountLoader<'info, UserLedger>,
    trade_cash: u64,
    is_taker: bool,
    taker_fee: u64,
) -> Result<()> {
    let total = if is_taker {
        trade_cash
            .checked_add(taker_fee)
            .ok_or(ErrorCode::MathOverflow)?
    } else {
        trade_cash
    };
    let mut ledger = ledger_loader.load_mut()?;
    require!(
        ledger.available_usdc >= total,
        ErrorCode::InsufficientCollateral
    );
    ledger.available_usdc = ledger
        .available_usdc
        .checked_sub(total)
        .ok_or(ErrorCode::MathOverflow)?;
    Ok(())
}

fn credit_seller<'info>(
    ledger_loader: &AccountLoader<'info, UserLedger>,
    trade_cash: u64,
    is_taker: bool,
    taker_fee: u64,
) -> Result<()> {
    let net = if is_taker {
        require!(trade_cash >= taker_fee, ErrorCode::MathOverflow);
        trade_cash
            .checked_sub(taker_fee)
            .ok_or(ErrorCode::MathOverflow)?
    } else {
        trade_cash
    };
    let mut ledger = ledger_loader.load_mut()?;
    ledger.available_usdc = ledger
        .available_usdc
        .checked_add(net)
        .ok_or(ErrorCode::MathOverflow)?;
    Ok(())
}

fn credit_position<'info>(
    position_loader: &AccountLoader<'info, UserPosition>,
    outcome: Outcome,
    delta: i128,
) -> Result<()> {
    let mut position = position_loader.load_mut()?;
    let target = match outcome {
        Outcome::Yes => &mut position.yes_shares,
        Outcome::No => &mut position.no_shares,
    };
    *target = apply_signed_delta(*target, delta)?;
    Ok(())
}

fn apply_market_delta(
    market: &mut Account<MarketState>,
    yes_delta: i128,
    no_delta: i128,
    volume_delta: u64,
    creator_fee_delta: u64,
    platform_fee_delta: u64,
) -> Result<()> {
    market.total_yes_open_interest = apply_signed_delta(market.total_yes_open_interest, yes_delta)?;
    market.total_no_open_interest = apply_signed_delta(market.total_no_open_interest, no_delta)?;
    market.total_matched_amount = market
        .total_matched_amount
        .checked_add(volume_delta)
        .ok_or(ErrorCode::MathOverflow)?;
    market.creator_unclaimed_fee = market
        .creator_unclaimed_fee
        .checked_add(creator_fee_delta)
        .ok_or(ErrorCode::MathOverflow)?;
    market.platform_unclaimed_fee = market
        .platform_unclaimed_fee
        .checked_add(platform_fee_delta)
        .ok_or(ErrorCode::MathOverflow)?;
    Ok(())
}

fn trade_cash_for_order(order: &OrderIntentV1, qty: u64, fill_price: u64) -> Result<u64> {
    let effective_price = match order.outcome {
        Outcome::Yes => fill_price,
        Outcome::No => 100u64
            .checked_sub(fill_price)
            .ok_or(ErrorCode::MathOverflow)?,
    };
    ceil_mul_div(qty, effective_price, 100)
}

fn ceil_mul_div(lhs: u64, rhs: u64, denominator: u64) -> Result<u64> {
    require!(denominator > 0, ErrorCode::MathOverflow);
    let numerator = (lhs as u128)
        .checked_mul(rhs as u128)
        .ok_or(ErrorCode::MathOverflow)?;
    let denominator = denominator as u128;
    let value = numerator
        .checked_add(denominator - 1)
        .ok_or(ErrorCode::MathOverflow)?
        / denominator;
    u64::try_from(value).map_err(|_| error!(ErrorCode::MathOverflow))
}

fn apply_signed_delta(value: u64, delta: i128) -> Result<u64> {
    if delta >= 0 {
        value
            .checked_add(delta as u64)
            .ok_or_else(|| error!(ErrorCode::MathOverflow))
    } else {
        value
            .checked_sub((-delta) as u64)
            .ok_or_else(|| error!(ErrorCode::MathOverflow))
    }
}

fn order_hash(order: &OrderIntentV1) -> Result<[u8; 32]> {
    let encoded = order.try_to_vec()?;
    Ok(solana_program::keccak::hash(&encoded).to_bytes())
}

fn order_signed_message_bytes(order: &OrderIntentV1) -> Result<Vec<u8>> {
    let hash = order_hash(order)?;
    Ok(lower_hex_bytes(&hash))
}

fn lower_hex_bytes(bytes: &[u8]) -> Vec<u8> {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut out = Vec::with_capacity(bytes.len() * 2);
    for byte in bytes {
        out.push(HEX[(byte >> 4) as usize]);
        out.push(HEX[(byte & 0x0f) as usize]);
    }
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    fn sample_order(side: Side, outcome: Outcome) -> OrderIntentV1 {
        OrderIntentV1 {
            version: 1,
            chain_id: 245,
            program_id: crate::ID,
            market: Pubkey::new_unique(),
            user: Pubkey::new_unique(),
            nonce: 7,
            side,
            outcome,
            order_type: OrderType::Limit,
            limit_price: 60,
            total_amount: 100,
            expiry_ts: i64::MAX,
        }
    }

    #[test]
    fn ceil_mul_div_rounds_up() {
        assert_eq!(ceil_mul_div(1, 1, 100).unwrap(), 1);
        assert_eq!(ceil_mul_div(101, 60, 100).unwrap(), 61);
    }

    #[test]
    fn classify_match_and_mint_requires_opposite_outcomes() {
        let maker = sample_order(Side::Buy, Outcome::Yes);
        let taker = sample_order(Side::Buy, Outcome::No);
        assert!(matches!(
            classify_branch(&maker, &taker).unwrap(),
            SettleBranch::MatchAndMint
        ));
    }

    #[test]
    fn trade_cash_for_no_uses_mirrored_price() {
        let order = sample_order(Side::Buy, Outcome::No);
        assert_eq!(trade_cash_for_order(&order, 10, 60).unwrap(), 4);
    }

    #[test]
    fn market_buy_progress_tracks_spend_units() {
        let mut order = sample_order(Side::Buy, Outcome::Yes);
        order.order_type = OrderType::Market;
        order.total_amount = 60;

        assert_eq!(order_progress_amount(&order, 100, 60).unwrap(), 60);
    }

    #[test]
    fn market_order_still_respects_protection_price() {
        let mut order = sample_order(Side::Buy, Outcome::Yes);
        order.order_type = OrderType::Market;
        order.limit_price = 59;

        assert!(validate_fill_price_for_order(&order, 60).is_err());
        assert!(validate_fill_price_for_order(&order, 59).is_ok());
    }

    #[test]
    fn ed25519_parser_matches_expected_message() {
        let order = sample_order(Side::Buy, Outcome::Yes);
        let message = order_signed_message_bytes(&order).unwrap();
        let pubkey = order.user.to_bytes();
        let signature = [7u8; 64];

        let signature_offset = 16u16;
        let public_key_offset = signature_offset + 64;
        let message_offset = public_key_offset + 32;

        let mut ix = vec![1u8, 0u8];
        ix.extend_from_slice(&signature_offset.to_le_bytes());
        ix.extend_from_slice(&u16::MAX.to_le_bytes());
        ix.extend_from_slice(&public_key_offset.to_le_bytes());
        ix.extend_from_slice(&u16::MAX.to_le_bytes());
        ix.extend_from_slice(&message_offset.to_le_bytes());
        ix.extend_from_slice(&(message.len() as u16).to_le_bytes());
        ix.extend_from_slice(&u16::MAX.to_le_bytes());
        ix.extend_from_slice(&signature);
        ix.extend_from_slice(&pubkey);
        ix.extend_from_slice(&message);

        assert!(ed25519_instruction_matches(&ix, &pubkey, &message).unwrap());
    }
}
