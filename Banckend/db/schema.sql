-- BlinkPredict current minimum schema
--
-- Purpose:
--   This file is the minimum Postgres baseline for the current manual test target:
--   page/dev sandbox -> submit limit order -> funds reserve -> offchain match
--   -> writer/query projection -> onchain settlement confirm.
--
-- Scope intentionally matches the current codebase, not the old idealized spec.
-- Auth challenge/login is currently in-memory and does not require database tables.
--
-- Required runtime modules covered here:
--   markets, faucet, deposit projection, funds recovery, writer, query API,
--   matcher recovery, settlement registry.
--
-- Notes:
--   1. `wallet_accounts` is the wallet-level trading projection used by funds recovery.
--   2. `positions` is the market-level position projection. Pending lots stay pending until
--      settlement confirmed; they must not be exposed as free lots beforehand.
--   3. `funds_recovery_state` stores the funds module checkpoint/inflight metadata so
--      cold start can recover from Postgres baseline + JetStream tail replay.

BEGIN;

CREATE TABLE IF NOT EXISTS markets (
    id UUID PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL UNIQUE,
    market_pda TEXT NOT NULL UNIQUE,
    metadata_cid TEXT NOT NULL DEFAULT '',
    metadata_url TEXT NOT NULL DEFAULT '',
    collateral_mint TEXT NOT NULL DEFAULT '',
    collateral_vault TEXT NOT NULL DEFAULT '',
    yes_mint TEXT NOT NULL DEFAULT '',
    no_mint TEXT NOT NULL DEFAULT '',
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL DEFAULT '',
    image_url TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL,
    outcome TEXT NOT NULL,
    resolution_mode TEXT NOT NULL,
    resolution_authority TEXT NOT NULL DEFAULT '',
    oracle_feed TEXT NOT NULL DEFAULT '',
    oracle_condition TEXT NOT NULL DEFAULT '',
    oracle_target_price BIGINT NOT NULL DEFAULT 0,
    oracle_target_expo INTEGER NOT NULL DEFAULT 0,
    close_time TIMESTAMPTZ NOT NULL,
    resolve_after_time TIMESTAMPTZ NOT NULL,
    claim_deadline_time TIMESTAMPTZ NOT NULL,
    creator_unclaimed_fee BIGINT NOT NULL DEFAULT 0 CHECK (creator_unclaimed_fee >= 0),
    platform_unclaimed_fee BIGINT NOT NULL DEFAULT 0 CHECK (platform_unclaimed_fee >= 0),
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_markets_created_at_desc
    ON markets (created_at DESC);

CREATE INDEX IF NOT EXISTS idx_markets_status_created_at
    ON markets (status, created_at DESC);

CREATE TABLE IF NOT EXISTS faucet_claims (
    id BIGSERIAL PRIMARY KEY,
    solana_address TEXT NOT NULL,
    ip TEXT NOT NULL,
    signature TEXT NOT NULL,
    amount BIGINT NOT NULL CHECK (amount > 0),
    mint TEXT NOT NULL,
    ata TEXT NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS faucet_claims_solana_address_claimed_at_idx
    ON faucet_claims (solana_address, claimed_at DESC);

CREATE INDEX IF NOT EXISTS faucet_claims_ip_claimed_at_idx
    ON faucet_claims (ip, claimed_at DESC);

CREATE TABLE IF NOT EXISTS consumer_cursors (
    consumer_name VARCHAR(64) NOT NULL,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    last_evt_seq BIGINT NOT NULL,
    last_source_cmd_seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer_name, market_id)
);

CREATE TABLE IF NOT EXISTS wallet_accounts (
    wallet_address VARCHAR(44) PRIMARY KEY,
    collateral_total_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_total_units >= 0),
    collateral_free_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_free_units >= 0),
    collateral_locked_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_locked_units >= 0),
    collateral_pending_units BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS funds_recovery_state (
    recovery_id SMALLINT PRIMARY KEY,
    last_flushed_evt_seq BIGINT NOT NULL DEFAULT 0,
    inflight_json JSONB NOT NULL DEFAULT '[]'::JSONB,
    pending_terminals_json JSONB NOT NULL DEFAULT '[]'::JSONB,
    pending_reserves_json JSONB NOT NULL DEFAULT '[]'::JSONB,
    processed_submits_json JSONB NOT NULL DEFAULT '[]'::JSONB,
    processed_deposits_json JSONB NOT NULL DEFAULT '[]'::JSONB,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS deposit_requests (
    id UUID PRIMARY KEY,
    wallet_address VARCHAR(44) NOT NULL,
    amount_units BIGINT NOT NULL CHECK (amount_units > 0),
    mint TEXT NOT NULL,
    treasury_destination TEXT NOT NULL,
    chain_signature TEXT,
    status TEXT NOT NULL,
    source TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_deposit_requests_wallet_created
    ON deposit_requests (wallet_address, created_at DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_deposit_requests_chain_signature
    ON deposit_requests (chain_signature)
    WHERE chain_signature IS NOT NULL AND chain_signature <> '';

CREATE TABLE IF NOT EXISTS deposit_submissions (
    signature TEXT PRIMARY KEY,
    wallet_address VARCHAR(44) NOT NULL,
    amount_units BIGINT NOT NULL CHECK (amount_units > 0),
    status TEXT NOT NULL CHECK (status IN ('submitted', 'watching', 'confirmed', 'failed', 'expired')),
    failure_reason TEXT NOT NULL DEFAULT '',
    slot BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_deposit_submissions_status_created
    ON deposit_submissions (status, created_at);

CREATE INDEX IF NOT EXISTS idx_deposit_submissions_wallet_created
    ON deposit_submissions (wallet_address, created_at DESC);

CREATE TABLE IF NOT EXISTS market_submissions (
    signature TEXT PRIMARY KEY,
    market_id NUMERIC(20,0),
    market_pda TEXT NOT NULL DEFAULT '',
    creator_wallet TEXT NOT NULL DEFAULT '',
    metadata_cid TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL CHECK (status IN ('submitted', 'watching', 'confirmed', 'failed', 'expired')),
    failure_reason TEXT NOT NULL DEFAULT '',
    slot BIGINT NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    confirmed_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_market_submissions_status_created
    ON market_submissions (status, created_at);

CREATE INDEX IF NOT EXISTS idx_market_submissions_market_created
    ON market_submissions (market_id, created_at DESC);

CREATE TABLE IF NOT EXISTS settlement_submissions (
    match_event_id TEXT PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    market_pda TEXT NOT NULL,
    status TEXT NOT NULL CHECK (status IN ('queued', 'prepared', 'submitted', 'processed', 'confirmed', 'failed')),
    market_lane_status TEXT NOT NULL DEFAULT 'active' CHECK (market_lane_status IN ('active', 'paused')),
    match_event_json JSONB NOT NULL,
    prepared_payload JSONB NOT NULL DEFAULT '{}'::jsonb,
    wallets_json JSONB NOT NULL,
    tx_signature TEXT NOT NULL DEFAULT '',
    raw_tx_base64 TEXT NOT NULL DEFAULT '',
    last_valid_block_height BIGINT NOT NULL DEFAULT 0,
    retry_count INT NOT NULL DEFAULT 0,
    processed_slot BIGINT NOT NULL DEFAULT 0,
    confirmation_slot BIGINT NOT NULL DEFAULT 0,
    reason_code TEXT NOT NULL DEFAULT '',
    submitted_event_published BOOLEAN NOT NULL DEFAULT FALSE,
    terminal_event_published BOOLEAN NOT NULL DEFAULT FALSE,
    prepared_at TIMESTAMPTZ,
    processed_at TIMESTAMPTZ,
    confirmed_at TIMESTAMPTZ,
    version BIGINT NOT NULL DEFAULT 1,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX IF NOT EXISTS settlement_submissions_tx_signature_uidx
    ON settlement_submissions (tx_signature)
    WHERE tx_signature <> '';

CREATE INDEX IF NOT EXISTS settlement_submissions_hot_queue_idx
    ON settlement_submissions (market_id, created_at)
    WHERE status = 'queued' AND market_lane_status = 'active';

CREATE INDEX IF NOT EXISTS settlement_submissions_hot_prepared_idx
    ON settlement_submissions (market_id, created_at)
    WHERE status = 'prepared' AND market_lane_status = 'active';

CREATE INDEX IF NOT EXISTS settlement_submissions_hot_submitted_idx
    ON settlement_submissions (updated_at)
    WHERE status = 'submitted';

CREATE INDEX IF NOT EXISTS settlement_submissions_hot_processed_idx
    ON settlement_submissions (updated_at)
    WHERE status = 'processed';

CREATE INDEX IF NOT EXISTS settlement_submissions_terminal_publish_idx
    ON settlement_submissions (updated_at)
    WHERE status IN ('confirmed', 'failed') AND terminal_event_published = FALSE;

CREATE TABLE IF NOT EXISTS orders (
    order_id BIGINT PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    wallet_address VARCHAR(44) NOT NULL,
    original_action TEXT NOT NULL,
    original_outcome TEXT NOT NULL,
    original_price_tick SMALLINT NOT NULL CHECK (original_price_tick BETWEEN 1 AND 99),
    side SMALLINT NOT NULL,
    order_type SMALLINT NOT NULL,
    price_tick SMALLINT NOT NULL CHECK (price_tick BETWEEN 1 AND 99),
    initial_qty BIGINT NOT NULL DEFAULT 0 CHECK (initial_qty >= 0),
    initial_spend_amount BIGINT NOT NULL DEFAULT 0 CHECK (initial_spend_amount >= 0),
    remaining_qty BIGINT NOT NULL CHECK (remaining_qty >= 0),
    remaining_spend_amount BIGINT NOT NULL DEFAULT 0 CHECK (remaining_spend_amount >= 0),
    expire_time BIGINT NOT NULL DEFAULT 0,
    status SMALLINT NOT NULL,
    signature TEXT NOT NULL,
    intent_hex TEXT NOT NULL,
    nonce BIGINT NOT NULL CHECK (nonce >= 0),
    created_cmd_seq BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_orders_active_recovery
    ON orders (market_id, side, price_tick, created_cmd_seq ASC)
    WHERE status IN (1, 2);

CREATE INDEX IF NOT EXISTS idx_orders_user_history
    ON orders (wallet_address, market_id, created_at DESC);

CREATE TABLE IF NOT EXISTS trades (
    trade_id VARCHAR(64) PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    source_cmd_seq BIGINT NOT NULL,
    match_price SMALLINT NOT NULL CHECK (match_price BETWEEN 1 AND 99),
    match_qty BIGINT NOT NULL CHECK (match_qty > 0),
    maker_order_id BIGINT NOT NULL,
    taker_order_id BIGINT NOT NULL,
    maker_wallet_address VARCHAR(44) NOT NULL,
    taker_wallet_address VARCHAR(44) NOT NULL,
    maker_signature TEXT NOT NULL,
    taker_signature TEXT NOT NULL,
    maker_intent_hex TEXT NOT NULL,
    taker_intent_hex TEXT NOT NULL,
    executed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_trades_market_exec
    ON trades (market_id, executed_at DESC);

CREATE INDEX IF NOT EXISTS idx_trades_maker_wallet
    ON trades (maker_wallet_address, executed_at DESC);

CREATE INDEX IF NOT EXISTS idx_trades_taker_wallet
    ON trades (taker_wallet_address, executed_at DESC);

CREATE TABLE IF NOT EXISTS positions (
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    wallet_address VARCHAR(44) NOT NULL,
    market_pda TEXT NOT NULL DEFAULT '',
    yes_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_free_lots >= 0),
    yes_locked_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_locked_lots >= 0),
    yes_pending_lots BIGINT NOT NULL DEFAULT 0,
    no_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_free_lots >= 0),
    no_locked_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_locked_lots >= 0),
    no_pending_lots BIGINT NOT NULL DEFAULT 0,
    collateral_locked_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_locked_units >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address)
);

CREATE INDEX IF NOT EXISTS idx_positions_wallet
    ON positions (wallet_address);

CREATE UNIQUE INDEX IF NOT EXISTS positions_wallet_pda_uidx
    ON positions (wallet_address, market_pda)
    WHERE market_pda <> '';

CREATE TABLE IF NOT EXISTS user_position_accounts (
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    wallet_address VARCHAR(44) NOT NULL,
    user_position_pda VARCHAR(44) NOT NULL,
    created_by_relayer VARCHAR(44),
    created_tx_sig TEXT,
    first_confirmed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address),
    UNIQUE (user_position_pda)
);

CREATE INDEX IF NOT EXISTS idx_user_position_accounts_wallet
    ON user_position_accounts (wallet_address);

CREATE TABLE IF NOT EXISTS order_state_accounts (
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    wallet_address VARCHAR(44) NOT NULL,
    nonce BIGINT NOT NULL CHECK (nonce >= 0),
    order_state_pda VARCHAR(44) NOT NULL,
    created_by_relayer VARCHAR(44),
    created_tx_sig TEXT,
    first_observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_observed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address, nonce),
    UNIQUE (order_state_pda)
);

CREATE INDEX IF NOT EXISTS idx_order_state_accounts_wallet
    ON order_state_accounts (wallet_address);

COMMIT;
