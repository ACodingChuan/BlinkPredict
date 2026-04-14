# BlinkPredict

BlinkPredict is a Solana-based prediction market system with a separated frontend/backend architecture, an event-driven backend pipeline, and an Anchor smart contract.
The repository currently uses a multi-module monorepo layout (`Frontened` / `Banckend` / `Contract`) and focuses on the core path from order submission to reserve, matching, on-chain settlement, and read-model projection.

## Repository Layout

```text
.
├── Banckend/   # Go API + NATS JetStream workers + PG/Redis projector
├── Frontened/  # Next.js 16 + React 19 trading frontend
├── Contract/   # Solana Anchor Program (Rust)
└── spec/       # Architecture and phased delivery docs
```

## System Architecture

```mermaid
flowchart LR
  U["User + Solana Wallet"]

  subgraph FE["Frontened (Next.js)"]
    UI["Trading UI / Wallet Adapter"]
    META["/api/market-metadata"]
  end

  subgraph BE["Banckend (Go)"]
    API["HTTP Gateway (chi)"]
    AUTH["Wallet Challenge Auth"]
    HUB["WebSocket Hub"]
    CMD["Command Publisher"]

    subgraph BUS["NATS"]
      JS["JetStream (AP_CMD/AP_EVT/AP_WHK)"]
      CORE["Core NATS hot fan-out"]
    end

    FUNDS["Funds Service"]
    MATCHER["Matcher (Market Actors)"]
    WRITER["Writer (PG/Redis projection)"]
    PUSHER["Pusher Service"]
    SETTLE["Settlement Service"]
    CFM["Confirm Workers (deposit/withdraw/market)"]
    MPJ["Market Projector"]
  end

  PG[("PostgreSQL")]
  REDIS[("Redis")]
  RPC["Solana RPC / WS"]
  PROG["Predix Anchor Program"]
  SPL["SPL Token Accounts (vUSDC / Vault)"]
  IPFS["IPFS Gateway"]
  HERMES["Pyth Hermes API"]

  U --> UI
  UI -->|REST /api| API
  UI -->|WS /ws/markets/:id| HUB
  UI -->|Sign message/tx| U
  UI -->|Validate feed id| HERMES
  UI --> META
  META -->|ipfs:// -> https://ipfs.io/ipfs/...| IPFS

  API --> AUTH
  API --> CMD
  CMD --> JS
  JS --> FUNDS
  FUNDS -->|evt.order.reserved / rejected| JS
  JS --> MATCHER

  MATCHER -->|evt.market.delta| JS
  MATCHER -->|hot.market.delta| CORE
  CORE --> PUSHER
  PUSHER --> HUB

  MATCHER -->|evt.match.execution| JS
  JS --> SETTLE
  SETTLE -->|submit + confirm| RPC
  RPC --> PROG
  PROG --> SPL
  SETTLE -->|evt.settlement.*| JS
  JS --> FUNDS
  JS --> MATCHER

  JS --> WRITER
  WRITER --> PG
  WRITER --> REDIS
  API --> PG
  API --> REDIS

  JS --> CFM
  CFM --> RPC
  CFM -->|evt.deposit/withdraw/market.*| JS
  JS --> FUNDS
  JS --> MPJ
  MPJ --> PG
  MPJ --> REDIS
```

## Core Order-to-Settlement Sequence

```mermaid
sequenceDiagram
  participant User as User/Wallet
  participant FE as Frontend
  participant API as Backend API
  participant NATS as NATS JetStream
  participant Funds as Funds
  participant Matcher as Matcher
  participant Writer as Writer
  participant Settlement as Settlement
  participant Solana as Solana RPC/Program

  User->>FE: Create order and sign order intent
  FE->>API: POST /api/orders (Bearer + Idempotency-Key + X-Trace-Id)
  API->>NATS: cmd.order.submit
  NATS->>Funds: Consume submit and run reserve check
  Funds->>NATS: evt.order.reserved or evt.order.reserve_rejected
  NATS->>Matcher: Consume reserved order
  Matcher->>NATS: evt.market.delta + evt.match.execution + evt.order.released
  NATS->>Writer: Project orders/trades/depth to PG/Redis
  NATS->>Settlement: Consume evt.match.execution
  Settlement->>Solana: Submit settle_match_batch transaction
  Solana-->>Settlement: confirmed / failed
  Settlement->>NATS: evt.settlement.submitted/confirmed/failed
  NATS->>Funds: Apply pending settlement transitions
  NATS->>Matcher: Consume settlement.confirmed to sync order-state cache
```

## Module Responsibilities

| Module | Responsibility | Key Dependencies |
|---|---|---|
| `Frontened` | Market UI, order placement, wallet login, market WS subscription | Next.js, wallet-adapter, zustand |
| `Banckend/internal/http` | Auth, signature validation, idempotent/traced command ingress | chi, auth, protocol |
| `Banckend/internal/funds` | Wallet-level funds state machine (`available/locked/pending`) | NATS, PG recovery, Redis projector |
| `Banckend/internal/matching` | Market-level actor matching and batch output | NATS Pull Consumer |
| `Banckend/internal/settlement` | On-chain settlement submit/confirm/retry and recovery | Solana RPC/WS, tx estimator |
| `Banckend/internal/writer` | Projection of orders/trades/depth into PG + Redis | PG, Redis |
| `Banckend/internal/pusher` | Real-time market delta fanout to WS clients | Core NATS, websocket hub |
| `Banckend/internal/*confirm` | deposit/withdraw/market confirm workers | Solana confirm waiter |
| `Contract/predix-program` | On-chain account model and `settle_match_batch` execution logic | Anchor, SPL Token, pyth-sdk-solana |

## External Integrations

- **Solana RPC/WS**: transaction submit, signature confirmation, and chain-state reads.
- **NATS JetStream**: durable command/event bus (`AP_CMD` / `AP_EVT` / `AP_WHK`).
- **Core NATS**: low-latency hot stream fanout (`hot.market.delta.*`).
- **PostgreSQL**: markets, orders, trades, accounts, and recovery-state persistence.
- **Redis**: high-frequency query read models and cache.
- **IPFS**: market metadata (CID) storage and retrieval (`ipfs://` normalization supported on both backend and frontend).
- **Pyth Hermes**: frontend feed-id and price validation for oracle-mode market creation.
- **Helius/Alchemy Webhook (optional)**: webhook handlers exist in code; default flow is currently confirm-worker driven.

## Contract Instructions (Current Codebase)

`Contract/programs/predix-program/src/lib.rs` currently includes:

- `initialize_config`
- `create_market`
- `init_user_position`
- `init_user_ledger`
- `deposit`
- `withdraw`
- `settle_match_batch`
- `close_empty_user_position`
- `close_empty_order_state`
- `close_user_ledger`
- `close_resolved_market`

## Quick Start (Local Development)

### 1) Backend

```bash
cd Banckend
cp .env.example .env
go run ./cmd/api
```

### 2) Frontend

```bash
cd Frontened
cp .env.example .env.local
npm install
npm run dev
```

### 3) Contract (Optional)

```bash
cd Contract
yarn install
anchor test
```

## Current Implementation Boundaries

- `/api/orders/split`, `/api/orders/merge`, and `/api/claims` are still transitional endpoints (partially scaffolded / placeholder behavior).
- Admin oracle resolve endpoints are reserved; full production-grade on-chain resolve automation can be extended from the current base.
- The event-driven core path is in place; next steps are multi-instance HA, full consumer migration cleanup, and additional on-chain policy hardening.
