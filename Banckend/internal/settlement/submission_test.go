package settlement

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"

	"blinkpredict/banckend/internal/matching"
	internalsolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
)

func makeMatchedOrder(
	programID solana.PublicKey,
	market solana.PublicKey,
	user solana.PublicKey,
	idx uint16,
	orderID uint64,
	side Side,
	outcome Outcome,
) matching.MatchedOrderV2 {
	intent := OrderIntentV1{
		Version:     1,
		ChainID:     245,
		ProgramID:   programID,
		Market:      market,
		User:        user,
		Side:        side,
		Outcome:     outcome,
		OrderType:   OrderTypeLimit,
		LimitPrice:  60,
		TotalAmount: 500,
		Nonce:       uint64(orderID),
		ExpiryTs:    123,
	}
	return matching.MatchedOrderV2{
		OrderIndex: idx,
		OrderID:    orderID,
		Execution: matching.ExecutionSnapshotV2{
			OrderID:           orderID,
			WalletAddress:     user.String(),
			OriginalAction:    map[Side]string{SideBuy: "buy", SideSell: "sell"}[side],
			OriginalOutcome:   map[Outcome]string{OutcomeYes: "yes", OutcomeNo: "no"}[outcome],
			OriginalPriceTick: 60,
			OrderType:         "limit",
			ExpireTime:        123,
			Nonce:             uint64(orderID),
		},
		Settlement: matching.SettlementPayloadV2{
			IntentBytesHex: hex.EncodeToString(intent.Serialize()),
			Signature:      base64.StdEncoding.EncodeToString(make([]byte, 64)),
		},
	}
}

func TestBuildSubmissionBatch(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	event := matching.MatchBatchEventV2{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrderV2{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFillV2{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}

	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: programID})
	if err != nil {
		t.Fatalf("BuildSubmissionBatch error: %v", err)
	}
	if len(batch.Orders) != 2 || len(batch.Fills) != 1 || len(batch.UniqueUsers) != 2 {
		t.Fatalf("unexpected batch sizes: %+v", batch)
	}
}

func TestBuildSubmissionBatchReturnsNoWorkWhenNoFills(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	user := solana.NewWallet().PublicKey()

	_, err := BuildSubmissionBatch(matching.MatchBatchEventV2{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders:    []matching.MatchedOrderV2{makeMatchedOrder(programID, market, user, 0, 1, SideBuy, OutcomeYes)},
	}, BuildConfig{ProgramID: programID})
	if !errors.Is(err, ErrNoSettlementWork) {
		t.Fatalf("expected ErrNoSettlementWork, got %v", err)
	}
}

func TestBuildSubmissionBatchSortsOrdersByOrderIndex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	event := matching.MatchBatchEventV2{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrderV2{
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
		},
		Fills: []matching.MatchFillV2{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}

	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: programID})
	if err != nil {
		t.Fatalf("BuildSubmissionBatch error: %v", err)
	}
	if batch.Orders[0].OrderIndex != 0 || batch.Orders[1].OrderIndex != 1 {
		t.Fatalf("orders not sorted by order_index: %#v", batch.Orders)
	}
	if !batch.UniqueUsers[0].Equals(userA) || !batch.UniqueUsers[1].Equals(userB) {
		t.Fatalf("unique users order mismatch: %#v", batch.UniqueUsers)
	}
}

func TestBuildSubmissionBatchRejectsMissingOrderIndex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	_, err := BuildSubmissionBatch(matching.MatchBatchEventV2{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrderV2{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 2, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFillV2{{MakerOrderIndex: 0, TakerOrderIndex: 2, FillAmount: 10, FillPrice: 60}},
	}, BuildConfig{ProgramID: programID})
	if err == nil {
		t.Fatal("expected error for non-contiguous order indexes")
	}
}

func TestBuildInstructionsOrdersSettlementAccountsByLayer(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	relayer := solana.NewWallet()
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	batch, err := BuildSubmissionBatch(matching.MatchBatchEventV2{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrderV2{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFillV2{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}, BuildConfig{ProgramID: programID})
	if err != nil {
		t.Fatalf("BuildSubmissionBatch error: %v", err)
	}

	positionA, err := internalsolana.DeriveUserPositionPDA(programID, userA, market)
	if err != nil {
		t.Fatalf("derive position A: %v", err)
	}
	instructions, err := (&Submitter{ProgramID: programID, Relayer: relayer.PrivateKey}).BuildInstructions(batch, UserPositionInitPlan{
		NeedInit: []UserPositionPlanEntry{{
			MarketID:      9,
			Wallet:        userA.String(),
			MarketPDA:     market,
			PositionPDA:   positionA,
			UserPublicKey: userA,
		}},
	})
	if err != nil {
		t.Fatalf("BuildInstructions error: %v", err)
	}
	if len(instructions) != 4 {
		t.Fatalf("unexpected instruction count: %d", len(instructions))
	}

	settleAccounts := instructions[len(instructions)-1].Accounts()
	if !settleAccounts[0].PublicKey.Equals(relayer.PublicKey()) {
		t.Fatalf("unexpected relayer account: %s", settleAccounts[0].PublicKey)
	}
	expectedLedgerA, _ := internalsolana.DeriveUserLedgerPDA(programID, userA)
	expectedLedgerB, _ := internalsolana.DeriveUserLedgerPDA(programID, userB)
	expectedPositionA, _ := internalsolana.DeriveUserPositionPDA(programID, userA, market)
	expectedPositionB, _ := internalsolana.DeriveUserPositionPDA(programID, userB, market)
	expectedOrderA, _ := internalsolana.DeriveOrderStatePDA(programID, userA, market, 1)
	expectedOrderB, _ := internalsolana.DeriveOrderStatePDA(programID, userB, market, 2)

	if !settleAccounts[5].PublicKey.Equals(expectedLedgerA) || !settleAccounts[6].PublicKey.Equals(expectedLedgerB) {
		t.Fatalf("ledger accounts out of order: %s %s", settleAccounts[5].PublicKey, settleAccounts[6].PublicKey)
	}
	if !settleAccounts[7].PublicKey.Equals(expectedPositionA) || !settleAccounts[8].PublicKey.Equals(expectedPositionB) {
		t.Fatalf("position accounts out of order: %s %s", settleAccounts[7].PublicKey, settleAccounts[8].PublicKey)
	}
	if !settleAccounts[9].PublicKey.Equals(expectedOrderA) || !settleAccounts[10].PublicKey.Equals(expectedOrderB) {
		t.Fatalf("order state accounts out of order: %s %s", settleAccounts[9].PublicKey, settleAccounts[10].PublicKey)
	}
}
