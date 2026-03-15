-- Faucet claims for vUSDC.
-- Enforces "one claim per wallet, per IP, per 24h" at the application layer.

create table if not exists faucet_claims (
  id bigserial primary key,
  solana_address text not null,
  ip text not null,
  signature text not null,
  amount bigint not null,
  mint text not null,
  ata text not null,
  claimed_at timestamptz not null default now()
);

create index if not exists faucet_claims_solana_address_claimed_at_idx
  on faucet_claims (solana_address, claimed_at desc);

create index if not exists faucet_claims_ip_claimed_at_idx
  on faucet_claims (ip, claimed_at desc);

