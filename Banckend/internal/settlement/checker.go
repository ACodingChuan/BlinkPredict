package settlement

import (
	"context"
	"fmt"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type RPCAccountExistenceChecker struct {
	Client *rpc.Client
}

func (c *RPCAccountExistenceChecker) AccountsExist(ctx context.Context, accounts []solana.PublicKey) (map[solana.PublicKey]bool, error) {
	result := make(map[solana.PublicKey]bool, len(accounts))
	if len(accounts) == 0 {
		return result, nil
	}
	if c == nil || c.Client == nil {
		return nil, fmt.Errorf("rpc client is not configured")
	}
	out, err := c.Client.GetMultipleAccounts(ctx, accounts...)
	if err != nil {
		return nil, fmt.Errorf("get multiple accounts: %w", err)
	}
	for i, account := range out.Value {
		result[accounts[i]] = account != nil
	}
	return result, nil
}
