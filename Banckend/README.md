# BlinkPredict Banckend

Go API skeleton for BlinkPredict v1a.

Current status:

- Market CRUD-style API is implemented with an in-memory repository.
- Matching endpoints intentionally return `matching_not_enabled`.
- Split / merge / claim / delegate / resolve endpoints reserve the correct API surface and tx-request tracking slots, but the actual Solana transaction builder is left for the next phase.
- `internal/matching` and `internal/indexer` already expose the interfaces needed for a later v1b implementation.
