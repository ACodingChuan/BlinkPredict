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
    market_id BIGINT NOT NULL UNIQUE,
    market_pda TEXT NOT NULL UNIQUE,
    metadata_url TEXT NOT NULL,
    collateral_mint TEXT NOT NULL,
    collateral_vault TEXT NOT NULL UNIQUE,
    yes_mint TEXT NOT NULL UNIQUE,
    no_mint TEXT NOT NULL UNIQUE,
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL,
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

CREATE TABLE IF NOT EXISTS market_metadata_cache (
    market_id BIGINT PRIMARY KEY REFERENCES markets(market_id),
    title TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    category TEXT NOT NULL,
    image_url TEXT NOT NULL DEFAULT '',
    cached_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS tx_requests (
    id UUID PRIMARY KEY,
    kind TEXT NOT NULL,
    market_id BIGINT,
    status TEXT NOT NULL,
    payload JSONB NOT NULL DEFAULT '{}'::JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
