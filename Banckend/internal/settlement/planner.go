package settlement

import (
	"context"
	"fmt"
	"sort"

	internalsolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
)

// AccountExistenceChecker batches on-chain existence checks for unknown PDAs.
type AccountExistenceChecker interface {
	AccountsExist(ctx context.Context, accounts []solana.PublicKey) (map[solana.PublicKey]bool, error)
}

// UserPositionInitPlan is the settlement-side decision result for one submission batch.
type UserPositionInitPlan struct {
	KnownWallets   []string
	UnknownWallets []string
	NeedInit       []UserPositionPlanEntry
	AlreadyExists  []UserPositionPlanEntry
}

// UserPositionPlanEntry ties one wallet to its position PDA for a single market.
type UserPositionPlanEntry struct {
	MarketID      uint64
	Wallet        string
	MarketPDA     solana.PublicKey
	PositionPDA   solana.PublicKey
	UserPublicKey solana.PublicKey
}

func BuildUserPositionInitPlan(
	ctx context.Context,
	programID solana.PublicKey,
	marketID uint64,
	marketPDA solana.PublicKey,
	wallets []string,
	registry *UserPositionRegistry,
	checker AccountExistenceChecker,
) (UserPositionInitPlan, error) {
	if registry == nil {
		return UserPositionInitPlan{}, fmt.Errorf("registry is required")
	}

	unknownWallets := registry.FilterUnknown(marketID, wallets)
	knownWallets := subtractWallets(wallets, unknownWallets)

	plan := UserPositionInitPlan{
		KnownWallets:   knownWallets,
		UnknownWallets: unknownWallets,
	}
	if len(unknownWallets) == 0 {
		return plan, nil
	}
	if checker == nil {
		return UserPositionInitPlan{}, fmt.Errorf("checker is required when unknown wallets exist")
	}

	entries := make([]UserPositionPlanEntry, 0, len(unknownWallets))
	accounts := make([]solana.PublicKey, 0, len(unknownWallets))
	for _, wallet := range unknownWallets {
		userKey, err := solana.PublicKeyFromBase58(wallet)
		if err != nil {
			return UserPositionInitPlan{}, fmt.Errorf("parse wallet %s: %w", wallet, err)
		}
		positionPDA, err := internalsolana.DeriveUserPositionPDA(programID, userKey, marketPDA)
		if err != nil {
			return UserPositionInitPlan{}, fmt.Errorf("derive user position pda: %w", err)
		}
		entry := UserPositionPlanEntry{
			MarketID:      marketID,
			Wallet:        wallet,
			MarketPDA:     marketPDA,
			PositionPDA:   positionPDA,
			UserPublicKey: userKey,
		}
		entries = append(entries, entry)
		accounts = append(accounts, positionPDA)
	}

	existsMap, err := checker.AccountsExist(ctx, accounts)
	if err != nil {
		return UserPositionInitPlan{}, fmt.Errorf("check user positions existence: %w", err)
	}
	for _, entry := range entries {
		if existsMap[entry.PositionPDA] {
			plan.AlreadyExists = append(plan.AlreadyExists, entry)
			continue
		}
		plan.NeedInit = append(plan.NeedInit, entry)
	}
	sort.Slice(plan.AlreadyExists, func(i, j int) bool { return plan.AlreadyExists[i].Wallet < plan.AlreadyExists[j].Wallet })
	sort.Slice(plan.NeedInit, func(i, j int) bool { return plan.NeedInit[i].Wallet < plan.NeedInit[j].Wallet })
	return plan, nil
}

func subtractWallets(all []string, unknown []string) []string {
	unknownSet := make(map[string]struct{}, len(unknown))
	for _, wallet := range unknown {
		unknownSet[wallet] = struct{}{}
	}
	seen := make(map[string]struct{}, len(all))
	known := make([]string, 0, len(all))
	for _, wallet := range all {
		if wallet == "" {
			continue
		}
		if _, ok := seen[wallet]; ok {
			continue
		}
		seen[wallet] = struct{}{}
		if _, ok := unknownSet[wallet]; !ok {
			known = append(known, wallet)
		}
	}
	sort.Strings(known)
	return known
}
