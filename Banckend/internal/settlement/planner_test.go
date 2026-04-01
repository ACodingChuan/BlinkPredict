package settlement

import (
	"context"
	"testing"

	internalsolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
)

type fakeChecker struct {
	exists map[solana.PublicKey]bool
}

func (f fakeChecker) AccountsExist(_ context.Context, accounts []solana.PublicKey) (map[solana.PublicKey]bool, error) {
	result := make(map[solana.PublicKey]bool, len(accounts))
	for _, account := range accounts {
		result[account] = f.exists[account]
	}
	return result, nil
}

func TestBuildUserPositionInitPlan(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	marketPDA := solana.MustPublicKeyFromBase58("9f8dQx4QJm4M2o8S4s4QxEn1zh7AwBTtJU1vGjufH6G2")
	registry := NewUserPositionRegistry()

	alice := solana.NewWallet().PublicKey().String()
	bob := solana.NewWallet().PublicKey().String()
	carol := solana.NewWallet().PublicKey().String()
	registry.MarkExists(7, alice)

	bobKey := solana.MustPublicKeyFromBase58(bob)
	bobPosition, err := internalsolana.DeriveUserPositionPDA(programID, bobKey, marketPDA)
	if err != nil {
		t.Fatalf("derive bob position: %v", err)
	}

	plan, err := BuildUserPositionInitPlan(
		context.Background(),
		programID,
		7,
		marketPDA,
		[]string{alice, bob, carol, carol},
		registry,
		fakeChecker{exists: map[solana.PublicKey]bool{bobPosition: true}},
	)
	if err != nil {
		t.Fatalf("BuildUserPositionInitPlan error: %v", err)
	}
	if len(plan.KnownWallets) != 1 || plan.KnownWallets[0] != alice {
		t.Fatalf("unexpected known wallets: %#v", plan.KnownWallets)
	}
	if len(plan.AlreadyExists) != 1 || plan.AlreadyExists[0].Wallet != bob {
		t.Fatalf("unexpected already exists entries: %#v", plan.AlreadyExists)
	}
	if len(plan.NeedInit) != 1 || plan.NeedInit[0].Wallet != carol {
		t.Fatalf("unexpected need init entries: %#v", plan.NeedInit)
	}
}
