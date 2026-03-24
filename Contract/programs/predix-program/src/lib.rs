use anchor_lang::prelude::*;
use anchor_spl::{
    associated_token::AssociatedToken,
    token_2022::Token2022,
    token_interface::{Mint, TokenAccount},
};
use anchor_lang::accounts::interface_account::InterfaceAccount;

mod error;
mod state;

use error::ErrorCode;
use state::*;

declare_id!("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA");

#[program]
pub mod predix_program {
    use super::*;

    /// 初始化预测市场 - 仅保留市场创建功能
    ///
    /// 此函数创建一个新的预测市场，包括：
    /// - 市场元数据（metadata_uri）
    /// - Yes/No Token Mint
    /// - 抵押品金库（collateral_vault）
    /// - 市场时间设置（close_ts、resolve_after_ts）
    /// - 决议模式设置（Creator/Pyth）
    ///
    /// 注意：此版本只负责市场发布，不包含其他功能
    ///
    /// # 参数
    /// * `market_id` - 市场唯一标识符
    /// * `metadata_uri` - 市场元数据 URL
    /// * `close_ts` - 市场关闭时间（Unix 时间戳）
    /// * `resolve_after_ts` - 决议时间（Unix 时间戳）
    /// * `resolution_mode` - 决议模式（Creator 或 Pyth）
    /// * `oracle_feed_id` - Pyth 预言机 Feed ID（仅 Pyth 模式）
    /// * `oracle_condition` - 预言机条件（仅 Pyth 模式）
    /// * `oracle_target_price_int` - 目标价格整数部分（仅 Pyth 模式）
    /// * `oracle_target_expo` - 目标价格指数部分（仅 Pyth 模式）
    #[allow(clippy::too_many_arguments)]
    pub fn initialize_market(
        ctx: Context<InitializeMarket>,
        market_id: u64,
        metadata_uri: String,
        close_ts: i64,
        resolve_after_ts: i64,
        resolution_mode: ResolutionMode,
        oracle_feed_id: [u8; 32],
        oracle_condition: OracleCondition,
        oracle_target_price_int: u64,
        oracle_target_expo: i32,
    ) -> Result<()> {
        // 验证时间参数
        let now = Clock::get()?.unix_timestamp;
        require!(
            close_ts > now,
            ErrorCode::InvalidExpirationTimestamp
        );
        require!(
            resolve_after_ts >= close_ts,
            ErrorCode::InvalidExpirationTimestamp
        );

        // Pyth 模式额外验证
        if resolution_mode == ResolutionMode::Pyth {
            require!(!is_zero_feed_id(&oracle_feed_id), ErrorCode::InvalidOracleAccount);
            require!(oracle_target_price_int > 0, ErrorCode::InvalidAmount);
        }

        // 初始化市场账户
        let market = &mut ctx.accounts.market;
        market.authority = ctx.accounts.admin.key();
        market.metadata_uri = metadata_uri;
        market.collateral_vault = ctx.accounts.vault.key();
        market.collateral_mint = ctx.accounts.collateral_mint.key();
        market.yes_mint = ctx.accounts.yes_mint.key();
        market.no_mint = ctx.accounts.no_mint.key();
        market.market_id = market_id;
        market.yes_total = 0;
        market.no_total = 0;
        market.outcome = MarketOutcome::Undecided;
        market.close_ts = close_ts;
        market.resolve_after_ts = resolve_after_ts;
        market.resolution_mode = resolution_mode;
        market.oracle_feed_id = match resolution_mode {
            ResolutionMode::Pyth => oracle_feed_id,
            ResolutionMode::Creator => [0u8; 32],
        };
        market.oracle_condition = oracle_condition;
        market.oracle_target_price_int = oracle_target_price_int;
        market.oracle_target_expo = oracle_target_expo;
        market.resolved_at = 0;

        // 发出市场初始化事件 - Helius webhook 监听此事件
        emit!(MarketInitialized {
            market_id,
            market_pda: market.key(),
            authority: market.authority,
            collateral_mint: market.collateral_mint,
            collateral_vault: market.collateral_vault,
            metadata_uri: market.metadata_uri.clone(),
            yes_mint: market.yes_mint,
            no_mint: market.no_mint,
            close_ts,
            resolve_after_ts,
            resolution_mode,
            oracle_feed_id: market.oracle_feed_id,
            oracle_condition,
            oracle_target_price_int,
            oracle_target_expo,
        });

        Ok(())
    }
}

/// InitializeMarket 指令的账户结构
#[derive(Accounts)]
#[instruction(market_id: u64)]
pub struct InitializeMarket<'info> {
    /// 市场账户 - 使用 PDA 派生
    #[account(
        init,
        payer = admin,
        space = 8 + Market::INIT_SPACE,
        seeds = [b"market", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub market: Account<'info, Market>,

    /// 抵押品金库 - 存储 USDC
    #[account(
        init,
        payer = admin,
        token::mint = collateral_mint,
        token::authority = market,
        token::token_program = token_program,
        seeds = [b"collateral_vault", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub vault: InterfaceAccount<'info, TokenAccount>,

    /// Yes Token Mint - 代表"正确"的代币
    #[account(
        init,
        payer = admin,
        mint::decimals = collateral_mint.decimals,
        mint::authority = market,
        mint::token_program = token_program,
        seeds = [b"yes_mint", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub yes_mint: InterfaceAccount<'info, Mint>,

    /// No Token Mint - 代表"错误"的代币
    #[account(
        init,
        payer = admin,
        mint::decimals = collateral_mint.decimals,
        mint::authority = market,
        mint::token_program = token_program,
        seeds = [b"no_mint", &market_id.to_le_bytes()[..]],
        bump
    )]
    pub no_mint: InterfaceAccount<'info, Mint>,

    /// 抵押品 Mint (如 USDC) - 支持 Token-2022
    pub collateral_mint: InterfaceAccount<'info, Mint>,

    /// 管理员账户 - 支付创建费用
    #[account(mut)]
    pub admin: Signer<'info>,

    /// Token Program (支持 Token-2022)
    pub token_program: Program<'info, Token2022>,

    /// Associated Token Program
    pub associated_token_program: Program<'info, AssociatedToken>,

    /// System Program
    pub system_program: Program<'info, System>,
}

/// 检查 feed_id 是否全为零
fn is_zero_feed_id(value: &[u8; 32]) -> bool {
    value.iter().all(|item| *item == 0)
}
