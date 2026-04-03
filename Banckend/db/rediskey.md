# Redis Key Design

This file documents the Redis keys that the current codebase actually reads and writes for page testing and the dev trade sandbox.

## Scope

The list below is not the final target architecture. It is the current minimum Redis surface needed for:

- market list / market detail reads
- wallet and position snapshot reads
- orderbook / trades / open orders query reads
- websocket ticket issuance
- gateway soft precheck cache
- writer rebuild and incremental projection
- funds-aware limit-order testing after the new `submit -> reserved -> match -> settlement confirmed` main path

Auth challenge/login is not stored in Redis today. It is held in the backend process memory by `SessionManager`.

## Required Keys

| Key pattern | Type | Producer | Consumer | Purpose | TTL |
| --- | --- | --- | --- | --- | --- |
| `market:{market_id}` | Hash | `internal/cache/market_cache.go` | `/api/markets`, `/api/markets/{marketId}` | Cached market detail snapshot | 24h |
| `markets:all` | ZSET | `internal/cache/market_cache.go` | `/api/markets` | Market ids sorted by `created_at` desc | 24h |
| `markets:open` | ZSET | `internal/cache/market_cache.go` | `/api/markets?status=open` | Open market ids sorted by `created_at` desc | 24h |
| `markets:resolved` | ZSET | `internal/cache/market_cache.go` | `/api/markets?status=resolved` | Resolved market ids sorted by `created_at` desc | 24h |
| `wallet-account:{wallet_address}` | Hash | HTTP split/merge/claim flow, deposit projector, wallet lock helpers | `/api/wallet-account`, gateway soft balance checks | Trading account snapshot used by page and gateway soft checks | persistent |
| `position:{market_id}:{wallet_address}` | Hash | HTTP split/merge/claim flow, writer rebuild, writer incremental sync | `/api/positions/{marketId}` and sell-side precheck | Per-market projected position snapshot | persistent |
| `l2:depth:{market_id}` | Hash | writer rebuild + incremental update | `internal/query`, `/api/orderbook/{marketId}` | Level 2 depth snapshot; fields are `bid:{tick}` / `ask:{tick}` | persistent |
| `trades:latest:{market_id}` | List | writer rebuild + incremental update | `internal/query`, `/api/trades/{marketId}` | Latest trades hot list | persistent capped list |
| `price:history:{market_id}` | ZSET | writer rebuild + incremental update | `internal/query`, `/api/price-history/{marketId}` | Hot window trade price history, score = `executed_at` ms | persistent hot window |
| `user:orders:{wallet_address}` | ZSET | writer rebuild + incremental update | `internal/query`, `/api/orders/open/{marketId}` | User open-order ids sorted by `created_cmd_seq` | persistent |
| `order:info:{order_id}` | Hash | writer rebuild + incremental update | `internal/query`, `/api/orders/open/{marketId}` | Order projection payload for one order | persistent while open, 1h after close |
| `ws:ticket:{ticket}` | String | `internal/pusher/ticket_store.go` | websocket upgrade endpoints | One-time websocket auth ticket -> wallet mapping | 45s |
| `gateway:balance:vusdc:{wallet_address}` | String | `internal/http/server.go` | gateway soft collateral fallback | Short-lived ATA balance cache for fallback precheck only | 15s |

## Hash Field Layouts

### `wallet-account:{wallet_address}`

Type: Hash

Fields:

- `collateral_total_units`
- `collateral_free_units`
- `collateral_locked_units`
- `updated_at`

Note:

- current Postgres table `wallet_accounts` only persists `total/free`
- `collateral_locked_units` is used by Redis-side lock helper logic and may exist only in Redis
- funds authority is now in-process `funds.Manager`, so this hash is still a projection/cache, not settlement truth

### `position:{market_id}:{wallet_address}`

Type: Hash

Fields:

- `yes_free_lots`
- `yes_locked_lots`
- `no_free_lots`
- `no_locked_lots`
- `collateral_free_units`
- `collateral_locked_units`
- `updated_at`

### `l2:depth:{market_id}`

Type: Hash

Fields:

- `bid:{price_tick}` => remaining lots
- `ask:{price_tick}` => remaining lots

Example:

- `bid:60 = 700`
- `ask:61 = 900`

### `order:info:{order_id}`

Type: Hash

Fields written by writer today:

- `market_id`
- `wallet_address`
- `original_action`
- `original_outcome`
- `original_price_tick`
- `normalized_side`
- `normalized_price_tick`
- `side`
- `order_type`
- `price_tick`
- `initial_qty`
- `initial_qty_lots`
- `initial_spend_amount`
- `remaining_qty`
- `remaining_qty_lots`
- `remaining_spend_amount`
- `expire_time`
- `status`
- `status_text`
- `created_cmd_seq`
- `updated_at`

## Operational Notes

- Redis is currently a read-model and hot-cache layer, not the system of record.
- If Redis is empty at startup, writer rebuilds `l2:depth:*`, `position:*`, `user:orders:*`, `order:info:*`, `trades:latest:*`, and `price:history:*` from Postgres.
- `wallet-account:*` is not fully rebuilt by writer today. It is refreshed by deposit projection and selected HTTP mutation paths.
- Because the current code still mixes Redis wallet projection and onchain ledger truth, Redis wallet values must not be treated as final settlement authority.
- The new main write path no longer lets matcher own wallet balances. Funds reserve/match/settlement transitions happen outside Redis; Redis remains query-side projection and gateway soft-check cache only.
