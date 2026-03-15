package faucet

import "context"

type DisabledService struct{}

func (DisabledService) Claim(_ context.Context, _ string, _ string) (Result, error) {
	return Result{}, ErrFaucetNotConfigured
}

