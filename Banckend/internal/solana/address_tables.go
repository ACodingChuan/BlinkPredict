package solana

import (
	"context"
	"fmt"
	"strings"

	addresslookuptable "github.com/gagliardetto/solana-go/programs/address-lookup-table"
	gsolana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

func CopyAddressTables(src map[gsolana.PublicKey]gsolana.PublicKeySlice) map[gsolana.PublicKey]gsolana.PublicKeySlice {
	if len(src) == 0 {
		return nil
	}
	out := make(map[gsolana.PublicKey]gsolana.PublicKeySlice, len(src))
	for table, addresses := range src {
		copied := make(gsolana.PublicKeySlice, len(addresses))
		copy(copied, addresses)
		out[table] = copied
	}
	return out
}

func LoadAddressTables(ctx context.Context, rpcClient *rpc.Client, rawIDs []string) (map[gsolana.PublicKey]gsolana.PublicKeySlice, error) {
	if rpcClient == nil || len(rawIDs) == 0 {
		return nil, nil
	}
	tables := make(map[gsolana.PublicKey]gsolana.PublicKeySlice, len(rawIDs))
	for _, raw := range rawIDs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		tableID, err := gsolana.PublicKeyFromBase58(raw)
		if err != nil {
			return nil, fmt.Errorf("parse address lookup table id %q: %w", raw, err)
		}
		state, err := addresslookuptable.GetAddressLookupTable(ctx, rpcClient, tableID)
		if err != nil {
			return nil, fmt.Errorf("load address lookup table %s: %w", tableID.String(), err)
		}
		if state == nil || len(state.Addresses) == 0 {
			continue
		}
		addresses := make(gsolana.PublicKeySlice, len(state.Addresses))
		copy(addresses, state.Addresses)
		tables[tableID] = addresses
	}
	if len(tables) == 0 {
		return nil, nil
	}
	return tables, nil
}
