# BlinkPredict Banckend

Go API skeleton for BlinkPredict v1a.

Current status:

- Market metadata APIs read/write PostgreSQL directly (DB env required).
- Order placement/cancel now publishes command envelopes to NATS JetStream when configured.
- Matching execution is still intentionally disabled in-process.
- Split / merge / claim / delegate / resolve endpoints reserve the correct API surface and tx-request tracking slots, but the actual Solana transaction builder is left for the next phase.
- `internal/matching` and `internal/indexer` already expose the interfaces needed for a later v1b implementation.
