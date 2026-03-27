-- BlinkPredict v2 schema
-- Full reset + init script for the new architecture.
-- Notes:
-- 1) market_id uses NUMERIC(20,0) to safely store uint64-like IDs.
-- 2) This script is intended for full rebuild environments.
-- 3) Seed market row below uses the exact data provided by the user.

BEGIN;

CREATE TABLE users (
    id UUID PRIMARY KEY,
    subject TEXT NOT NULL UNIQUE,
    email TEXT,
    name TEXT,
    solana_address TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE markets (
    id UUID PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL UNIQUE,
    market_pda TEXT NOT NULL UNIQUE,
    metadata_url TEXT NOT NULL,
    condition_id TEXT NOT NULL DEFAULT '',
    collateral_mint TEXT NOT NULL,
    collateral_vault TEXT NOT NULL UNIQUE,
    yes_asset_id TEXT NOT NULL DEFAULT '',
    no_asset_id TEXT NOT NULL DEFAULT '',
    yes_mint TEXT NOT NULL UNIQUE,
    no_mint TEXT NOT NULL UNIQUE,
    tick_size SMALLINT NOT NULL DEFAULT 1 CHECK (tick_size > 0 AND tick_size <= 100),
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
    oracle_observation_time TIMESTAMPTZ,
    close_time TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_markets_status_close_time
    ON markets (status, close_time);

CREATE TABLE faucet_claims (
    id BIGSERIAL PRIMARY KEY,
    solana_address TEXT NOT NULL,
    ip TEXT NOT NULL,
    signature TEXT NOT NULL,
    amount BIGINT NOT NULL,
    mint TEXT NOT NULL,
    ata TEXT NOT NULL,
    claimed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_faucet_claims_wallet_claimed_at
    ON faucet_claims (solana_address, claimed_at DESC);

CREATE INDEX idx_faucet_claims_ip_claimed_at
    ON faucet_claims (ip, claimed_at DESC);

CREATE TABLE consumer_cursors (
    consumer_name VARCHAR(64) NOT NULL,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id) ON DELETE CASCADE,
    last_evt_seq BIGINT NOT NULL,
    last_source_cmd_seq BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (consumer_name, market_id)
);

CREATE TABLE orders (
    order_id BIGINT PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id) ON DELETE CASCADE,
    wallet_address VARCHAR(44) NOT NULL,
    asset_kind TEXT NOT NULL,
    side TEXT NOT NULL,
    order_type TEXT NOT NULL,
    price_tick SMALLINT NOT NULL CHECK (price_tick BETWEEN 1 AND 99),
    qty_lots BIGINT NOT NULL CHECK (qty_lots >= 0),
    spend_amount BIGINT NOT NULL DEFAULT 0 CHECK (spend_amount >= 0),
    remaining_qty BIGINT NOT NULL CHECK (remaining_qty >= 0),
    nonce NUMERIC(20,0) NOT NULL,
    signature TEXT NOT NULL,
    status TEXT NOT NULL,
    reservation_status TEXT NOT NULL DEFAULT 'none',
    settlement_status TEXT NOT NULL DEFAULT 'none',
    invalid_reason TEXT NOT NULL DEFAULT '',
    expire_time BIGINT NOT NULL DEFAULT 0,
    created_cmd_seq BIGINT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_orders_market_status_price_seq
    ON orders (market_id, status, price_tick, created_cmd_seq);

CREATE INDEX idx_orders_wallet_created_desc
    ON orders (wallet_address, created_at DESC);

CREATE INDEX idx_orders_wallet_market_status
    ON orders (wallet_address, market_id, status);

CREATE TABLE trades (
    trade_id VARCHAR(64) PRIMARY KEY,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id) ON DELETE CASCADE,
    maker_order_id BIGINT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    taker_order_id BIGINT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    maker_wallet_address VARCHAR(44) NOT NULL,
    taker_wallet_address VARCHAR(44) NOT NULL,
    match_price SMALLINT NOT NULL CHECK (match_price BETWEEN 1 AND 99),
    match_qty BIGINT NOT NULL CHECK (match_qty > 0),
    status TEXT NOT NULL,
    settlement_task_id UUID,
    chain_tx_sig TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    submitted_at TIMESTAMPTZ,
    confirmed_at TIMESTAMPTZ,
    failed_at TIMESTAMPTZ,
    failure_reason TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_trades_market_created_desc
    ON trades (market_id, created_at DESC);

CREATE INDEX idx_trades_maker_wallet_created_desc
    ON trades (maker_wallet_address, created_at DESC);

CREATE INDEX idx_trades_taker_wallet_created_desc
    ON trades (taker_wallet_address, created_at DESC);

CREATE INDEX idx_trades_status_created
    ON trades (status, created_at);

CREATE TABLE positions (
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id) ON DELETE CASCADE,
    wallet_address VARCHAR(44) NOT NULL,
    yes_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_free_lots >= 0),
    yes_reserved_open_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_reserved_open_lots >= 0),
    yes_pending_settlement_lots BIGINT NOT NULL DEFAULT 0 CHECK (yes_pending_settlement_lots >= 0),
    no_free_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_free_lots >= 0),
    no_reserved_open_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_reserved_open_lots >= 0),
    no_pending_settlement_lots BIGINT NOT NULL DEFAULT 0 CHECK (no_pending_settlement_lots >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (market_id, wallet_address)
);

CREATE INDEX idx_positions_wallet
    ON positions (wallet_address);

CREATE TABLE wallet_asset_balances (
    wallet_address VARCHAR(44) NOT NULL,
    asset_type TEXT NOT NULL,
    market_id NUMERIC(20,0),
    asset_id TEXT NOT NULL,
    confirmed_units BIGINT NOT NULL DEFAULT 0 CHECK (confirmed_units >= 0),
    delegated_units BIGINT NOT NULL DEFAULT 0 CHECK (delegated_units >= 0),
    last_observed_slot BIGINT NOT NULL DEFAULT 0 CHECK (last_observed_slot >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (wallet_address, asset_type, market_id)
);

CREATE INDEX idx_wallet_asset_balances_wallet
    ON wallet_asset_balances (wallet_address);

CREATE INDEX idx_wallet_asset_balances_market_asset
    ON wallet_asset_balances (market_id, asset_type);

CREATE TABLE wallet_reservation_state (
    wallet_address VARCHAR(44) NOT NULL,
    asset_type TEXT NOT NULL,
    market_id NUMERIC(20,0),
    reserved_open_units BIGINT NOT NULL DEFAULT 0 CHECK (reserved_open_units >= 0),
    reserved_pending_settlement_units BIGINT NOT NULL DEFAULT 0 CHECK (reserved_pending_settlement_units >= 0),
    version BIGINT NOT NULL DEFAULT 0 CHECK (version >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (wallet_address, asset_type, market_id)
);

CREATE TABLE wallet_reservations (
    reservation_id UUID PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    wallet_address VARCHAR(44) NOT NULL,
    asset_type TEXT NOT NULL,
    market_id NUMERIC(20,0),
    original_reserved_units BIGINT NOT NULL DEFAULT 0 CHECK (original_reserved_units >= 0),
    open_reserved_units BIGINT NOT NULL DEFAULT 0 CHECK (open_reserved_units >= 0),
    pending_settlement_units BIGINT NOT NULL DEFAULT 0 CHECK (pending_settlement_units >= 0),
    released_units BIGINT NOT NULL DEFAULT 0 CHECK (released_units >= 0),
    finalized_units BIGINT NOT NULL DEFAULT 0 CHECK (finalized_units >= 0),
    rolled_back_units BIGINT NOT NULL DEFAULT 0 CHECK (rolled_back_units >= 0),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_wallet_reservations_order_id
    ON wallet_reservations (order_id);

CREATE INDEX idx_wallet_reservations_wallet_market
    ON wallet_reservations (wallet_address, market_id);

CREATE INDEX idx_wallet_reservations_wallet_updated_desc
    ON wallet_reservations (wallet_address, updated_at DESC);

CREATE TABLE wallet_reservation_events (
    id BIGSERIAL PRIMARY KEY,
    reservation_id UUID NOT NULL REFERENCES wallet_reservations(reservation_id) ON DELETE CASCADE,
    order_id BIGINT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    wallet_address VARCHAR(44) NOT NULL,
    event_type TEXT NOT NULL,
    delta_units BIGINT NOT NULL,
    reason TEXT NOT NULL DEFAULT '',
    source_event_id TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_wallet_reservation_events_reservation_created
    ON wallet_reservation_events (reservation_id, created_at);

CREATE INDEX idx_wallet_reservation_events_order_created
    ON wallet_reservation_events (order_id, created_at);

CREATE TABLE settlement_tasks (
    task_id UUID PRIMARY KEY,
    trade_id VARCHAR(64) NOT NULL UNIQUE REFERENCES trades(trade_id) ON DELETE CASCADE,
    market_id NUMERIC(20,0) NOT NULL REFERENCES markets(market_id) ON DELETE CASCADE,
    status TEXT NOT NULL,
    attempt_count INT NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    next_retry_at TIMESTAMPTZ,
    chain_tx_sig TEXT,
    last_error TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_settlement_tasks_status_next_retry
    ON settlement_tasks (status, next_retry_at);

CREATE TABLE chain_observations (
    id BIGSERIAL PRIMARY KEY,
    wallet_address VARCHAR(44) NOT NULL,
    observation_type TEXT NOT NULL,
    slot BIGINT NOT NULL CHECK (slot >= 0),
    signature TEXT NOT NULL DEFAULT '',
    payload JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_chain_observations_wallet_slot_desc
    ON chain_observations (wallet_address, slot DESC);

CREATE INDEX idx_chain_observations_signature
    ON chain_observations (signature);

CREATE INDEX idx_chain_observations_type_slot_desc
    ON chain_observations (observation_type, slot DESC);

CREATE TABLE order_invalidations (
    id BIGSERIAL PRIMARY KEY,
    order_id BIGINT NOT NULL REFERENCES orders(order_id) ON DELETE CASCADE,
    wallet_address VARCHAR(44) NOT NULL,
    reason_code TEXT NOT NULL,
    observed_slot BIGINT NOT NULL CHECK (observed_slot >= 0),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_order_invalidations_order_id
    ON order_invalidations (order_id);

CREATE INDEX idx_order_invalidations_wallet_created_desc
    ON order_invalidations (wallet_address, created_at DESC);

INSERT INTO markets (
    id,
    market_id,
    market_pda,
    metadata_url,
    condition_id,
    collateral_mint,
    collateral_vault,
    yes_asset_id,
    no_asset_id,
    yes_mint,
    no_mint,
    tick_size,
    title,
    description,
    category,
    image_url,
    status,
    outcome,
    resolution_mode,
    resolution_authority,
    oracle_feed,
    oracle_condition,
    oracle_target_price,
    oracle_observation_time,
    close_time,
    resolved_at,
    created_at,
    updated_at
) VALUES (
    '38404a8a-3fdf-4c0b-a525-3cb550482eec',
    '17978006647606679225',
    '9BWEoGyTL1GDbe8tQ8Rg4me8GQzAkTtBJZ3YTCB215VL',
    'ipfs://QmWk6LeLYBXRuNMU3Qc5yYbkWZmKCVMGwrM2rf2km3ssyr',
    '',
    'E1Yy8CgMVYLSEpBPWR4ZTdwBKscAMKBYbPQXmatDBqCe',
    'DEjGxCwMjXMHLHcUX8GSBCJrRAPbWGFdbjnsppqRQFP1',
    'GafQdBxxPurzauN7w9cXDi7pKRwqJr9CLuHnXc5WMznB',
    'CduBCH6nA9q3nBhZLYnpDML3GuRZtCt1i6oV8wzT15Dj',
    'GafQdBxxPurzauN7w9cXDi7pKRwqJr9CLuHnXc5WMznB',
    'CduBCH6nA9q3nBhZLYnpDML3GuRZtCt1i6oV8wzT15Dj',
    1,
    '曼城赢',
    '联赛冠军 曼城 yes win',
    '',
    '',
    'open',
    'undecided',
    'Creator',
    '4zqSfau7F5qac2v3q1DLZULYy4jGoXqGTYK8N9h7EMPy',
    '0000000000000000000000000000000000000000000000000000000000000000',
    'GreaterThan',
    0,
    '2026-03-28 15:16:00+00',
    '2026-03-26 15:16:00+00',
    NULL,
    '2026-03-27 15:17:38.185717+00',
    '2026-03-26 15:17:38.185717+00'
);

COMMIT;
