# BlinkPredict Contract

Anchor program skeleton for BlinkPredict v1a.

Implemented instructions:

- `initialize_market`
- `split_token`
- `merge_tokens`
- `resolve_by_creator`
- `resolve_by_pyth`
- `claim_reward`

The contract keeps the existing SPL-token-based asset model, adds `Creator` and `Pyth` resolution modes, and intentionally leaves matching for a later phase.
