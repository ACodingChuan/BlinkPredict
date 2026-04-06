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

func assertAccountMetaFlags(t *testing.T, label string, meta *solana.AccountMeta, writable bool, signer bool) {
	t.Helper()
	if meta.IsWritable != writable || meta.IsSigner != signer {
		t.Fatalf("%s flags mismatch: writable=%v signer=%v", label, meta.IsWritable, meta.IsSigner)
	}
}

func makeMatchedOrder(
	programID solana.PublicKey,
	market solana.PublicKey,
	user solana.PublicKey,
	idx uint16,
	orderID uint64,
	side Side,
	outcome Outcome,
) matching.MatchedOrder {
	intent := OrderIntentV1{
		Version:     1,
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
	return matching.MatchedOrder{
		OrderIndex: idx,
		OrderID:    orderID,
		Execution: matching.ExecutionSnapshot{
			OrderID:           orderID,
			WalletAddress:     user.String(),
			OriginalAction:    map[Side]string{SideBuy: "buy", SideSell: "sell"}[side],
			OriginalOutcome:   map[Outcome]string{OutcomeYes: "yes", OutcomeNo: "no"}[outcome],
			OriginalPriceTick: 60,
			OrderType:         "limit",
			ExpireTime:        123,
			Nonce:             uint64(orderID),
		},
		Settlement: matching.SettlementPayload{
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

	event := matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrder{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
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

	_, err := BuildSubmissionBatch(matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders:    []matching.MatchedOrder{makeMatchedOrder(programID, market, user, 0, 1, SideBuy, OutcomeYes)},
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

	event := matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrder{
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
		},
		Fills: []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
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

func TestBuildSubmissionBatchAcceptsWhitespaceInIntentHex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	orderA := makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes)
	orderB := makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes)
	orderA.Settlement.IntentBytesHex = orderA.Settlement.IntentBytesHex[:40] + "\n  " + orderA.Settlement.IntentBytesHex[40:]

	event := matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders:    []matching.MatchedOrder{orderA, orderB},
		Fills:     []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}

	if _, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: programID}); err != nil {
		t.Fatalf("BuildSubmissionBatch should accept whitespace in intent hex: %v", err)
	}
}

func TestBuildSubmissionBatchRejectsMissingOrderIndex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	_, err := BuildSubmissionBatch(matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrder{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 2, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 2, FillAmount: 10, FillPrice: 60}},
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

	batch, err := BuildSubmissionBatch(matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrder{
			makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes),
			makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes),
		},
		Fills: []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
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

	initAccounts := instructions[2].Accounts()
	assertAccountMetaFlags(t, "init payer", initAccounts[0], true, true)
	assertAccountMetaFlags(t, "init config", initAccounts[1], false, false)
	assertAccountMetaFlags(t, "init user", initAccounts[2], false, false)
	assertAccountMetaFlags(t, "init market", initAccounts[3], false, false)
	assertAccountMetaFlags(t, "init user_position", initAccounts[4], true, false)
	assertAccountMetaFlags(t, "init system", initAccounts[5], false, false)

	settleAccounts := instructions[len(instructions)-1].Accounts()
	if !settleAccounts[0].PublicKey.Equals(relayer.PublicKey()) {
		t.Fatalf("unexpected relayer account: %s", settleAccounts[0].PublicKey)
	}
	assertAccountMetaFlags(t, "settle relayer", settleAccounts[0], true, true)
	assertAccountMetaFlags(t, "settle config", settleAccounts[1], false, false)
	assertAccountMetaFlags(t, "settle market", settleAccounts[2], true, false)
	assertAccountMetaFlags(t, "settle instruction sysvar", settleAccounts[3], false, false)
	assertAccountMetaFlags(t, "settle system", settleAccounts[4], false, false)
	expectedLedgerA, _ := internalsolana.DeriveUserLedgerPDA(programID, userA)
	expectedLedgerB, _ := internalsolana.DeriveUserLedgerPDA(programID, userB)
	expectedPositionA, _ := internalsolana.DeriveUserPositionPDA(programID, userA, market)
	expectedPositionB, _ := internalsolana.DeriveUserPositionPDA(programID, userB, market)
	expectedOrderA, _ := internalsolana.DeriveOrderStatePDA(programID, userA, market, 1)
	expectedOrderB, _ := internalsolana.DeriveOrderStatePDA(programID, userB, market, 2)

	if !settleAccounts[5].PublicKey.Equals(expectedLedgerA) || !settleAccounts[6].PublicKey.Equals(expectedLedgerB) {
		t.Fatalf("ledger accounts out of order: %s %s", settleAccounts[5].PublicKey, settleAccounts[6].PublicKey)
	}
	assertAccountMetaFlags(t, "settle ledger A", settleAccounts[5], true, false)
	assertAccountMetaFlags(t, "settle ledger B", settleAccounts[6], true, false)
	if !settleAccounts[7].PublicKey.Equals(expectedPositionA) || !settleAccounts[8].PublicKey.Equals(expectedPositionB) {
		t.Fatalf("position accounts out of order: %s %s", settleAccounts[7].PublicKey, settleAccounts[8].PublicKey)
	}
	assertAccountMetaFlags(t, "settle position A", settleAccounts[7], true, false)
	assertAccountMetaFlags(t, "settle position B", settleAccounts[8], true, false)
	if !settleAccounts[9].PublicKey.Equals(expectedOrderA) || !settleAccounts[10].PublicKey.Equals(expectedOrderB) {
		t.Fatalf("order state accounts out of order: %s %s", settleAccounts[9].PublicKey, settleAccounts[10].PublicKey)
	}
	assertAccountMetaFlags(t, "settle order state A", settleAccounts[9], true, false)
	assertAccountMetaFlags(t, "settle order state B", settleAccounts[10], true, false)
}
