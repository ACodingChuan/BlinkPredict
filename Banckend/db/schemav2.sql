-- BlinkPredict schema v2
--
-- Hot path for the current submit-order -> offchain match -> onchain settlement flow:
--   markets, consumer_cursors, wallet_accounts, deposit_requests, webhook_receipts,
--   orders, order_locks, trades, positions, user_position_accounts.
--
-- Compatibility tables retained:
--   users, tx_requests, faucet_claims.

BEGIN;

CREATE TABLE IF NOT EXISTS users (
    id UUID PRIMARY KEY,
    subject TEXT NOT NULL UNIQUE,
    email TEXT,
    name TEXT,
    solana_address TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

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

CREATE TABLE IF NOT EXISTS tx_requests (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL,
    market_id NUMERIC(20,0),
    status TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

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

CREATE TABLE IF NOT EXISTS webhook_receipts (
    event_id TEXT PRIMARY KEY,
    provider TEXT NOT NULL,
    event_type TEXT NOT NULL,
    signature TEXT,
    slot BIGINT NOT NULL DEFAULT 0,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    payload_json JSONB NOT NULL DEFAULT '{}'::JSONB
);

CREATE INDEX IF NOT EXISTS idx_webhook_receipts_provider_event_received
    ON webhook_receipts (provider, event_type, received_at DESC);

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

CREATE TABLE IF NOT EXISTS order_locks (
    order_id BIGINT PRIMARY KEY,
    wallet_address VARCHAR(44) NOT NULL,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id),
    locked_amount BIGINT NOT NULL CHECK (locked_amount >= 0),
    status TEXT NOT NULL CHECK (status IN ('pending', 'active', 'released', 'consumed')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_order_locks_wallet_status
    ON order_locks (wallet_address, status);

CREATE INDEX IF NOT EXISTS idx_order_locks_market_status
    ON order_locks (market_id, status);

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
    yes_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_free_lots >= 0),
    yes_locked_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_locked_lots >= 0),
    no_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_free_lots >= 0),
    no_locked_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_locked_lots >= 0),
    collateral_free_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_free_units >= 0),
    collateral_locked_units BIGINT NOT NULL DEFAULT 0 CHECK (collateral_locked_units >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address)
);

CREATE INDEX IF NOT EXISTS idx_positions_wallet
    ON positions (wallet_address);

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

COMMIT;
