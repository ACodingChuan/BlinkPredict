package settlement

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
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

	instructions, err := (&Submitter{ProgramID: programID, Relayer: relayer.PrivateKey}).BuildInstructions(batch)
	if err != nil {
		t.Fatalf("BuildInstructions error: %v", err)
	}
	if len(instructions) != 3 {
		t.Fatalf("unexpected instruction count: %d", len(instructions))
	}

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

func TestBuildSubmissionBatchRejectsInvalidSignatureLength(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	orderA := makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes)
	orderB := makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes)
	orderA.Settlement.Signature = base64.StdEncoding.EncodeToString(make([]byte, 63))

	_, err := BuildSubmissionBatch(matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders:    []matching.MatchedOrder{orderA, orderB},
		Fills:     []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}, BuildConfig{ProgramID: programID})
	if err == nil {
		t.Fatalf("expected BuildSubmissionBatch to reject non-64-byte signature")
	}
}

func TestBuildSubmissionBatchRejectsSubOneUsdcNotional(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	market := solana.NewWallet().PublicKey()
	userA := solana.NewWallet().PublicKey()
	userB := solana.NewWallet().PublicKey()

	orderA := makeMatchedOrder(programID, market, userA, 0, 1, SideBuy, OutcomeYes)
	orderB := makeMatchedOrder(programID, market, userB, 1, 2, SideSell, OutcomeYes)

	intentA := OrderIntentV1{
		ProgramID:   programID,
		Market:      market,
		User:        userA,
		Nonce:       1,
		Side:        SideBuy,
		Outcome:     OutcomeYes,
		OrderType:   OrderTypeLimit,
		LimitPrice:  99,
		TotalAmount: 100,
		ExpiryTs:    123,
	}
	orderA.Settlement.IntentBytesHex = hex.EncodeToString(intentA.Serialize())
	orderA.Execution.OriginalPriceTick = 99

	_, err := BuildSubmissionBatch(matching.MatchBatchEvent{
		MarketID:  9,
		MarketPDA: market.String(),
		Orders:    []matching.MatchedOrder{orderA, orderB},
		Fills:     []matching.MatchFill{{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 10, FillPrice: 60}},
	}, BuildConfig{ProgramID: programID})
	if err == nil {
		t.Fatalf("expected BuildSubmissionBatch to reject minimum notional below 100 units")
	}
}

func TestPreparedPayloadRoundTrip(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
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
	batch.Orders[1].Warm = true

	payload, err := encodePreparedPayload(batch)
	if err != nil {
		t.Fatalf("encodePreparedPayload error: %v", err)
	}
	decoded, err := decodePreparedPayload(payload, BuildConfig{ProgramID: programID})
	if err != nil {
		t.Fatalf("decodePreparedPayload error: %v", err)
	}

	if decoded.MarketID != batch.MarketID || !decoded.MarketPDA.Equals(batch.MarketPDA) {
		t.Fatalf("market mismatch after roundtrip")
	}
	if len(decoded.Orders) != len(batch.Orders) || len(decoded.Fills) != len(batch.Fills) || len(decoded.UniqueUsers) != len(batch.UniqueUsers) {
		t.Fatalf("roundtrip size mismatch")
	}
	for i := range batch.Orders {
		if decoded.Orders[i].OrderIndex != batch.Orders[i].OrderIndex {
			t.Fatalf("order index mismatch at %d", i)
		}
		if decoded.Orders[i].Warm != batch.Orders[i].Warm {
			t.Fatalf("order warm flag mismatch at %d", i)
		}
		if decoded.Orders[i].Intent != batch.Orders[i].Intent {
			t.Fatalf("intent mismatch at %d", i)
		}
		if hex.EncodeToString(decoded.Orders[i].RawIntent) != hex.EncodeToString(batch.Orders[i].RawIntent) {
			t.Fatalf("raw intent mismatch at %d", i)
		}
		if base64.StdEncoding.EncodeToString(decoded.Orders[i].Signature) != base64.StdEncoding.EncodeToString(batch.Orders[i].Signature) {
			t.Fatalf("signature mismatch at %d", i)
		}
	}
}

func TestDecodePreparedPayloadRejectsNonContiguousOrderIndex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	payload := mustPreparedPayloadFromSingleFillBatch(t, programID)
	payload.Orders[1].OrderIndex = 3
	raw := mustMarshalPreparedPayload(t, payload)

	if _, err := decodePreparedPayload(raw, BuildConfig{ProgramID: programID}); err == nil {
		t.Fatalf("expected decodePreparedPayload to reject non-contiguous order indexes")
	}
}

func TestDecodePreparedPayloadRejectsOutOfRangeFillIndex(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	payload := mustPreparedPayloadFromSingleFillBatch(t, programID)
	payload.Fills[0].TakerIdx = 9
	raw := mustMarshalPreparedPayload(t, payload)

	if _, err := decodePreparedPayload(raw, BuildConfig{ProgramID: programID}); err == nil {
		t.Fatalf("expected decodePreparedPayload to reject out-of-range fill index")
	}
}

func TestDecodePreparedPayloadRejectsSelfMatchFill(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	payload := mustPreparedPayloadFromSingleFillBatch(t, programID)
	payload.Fills[0].TakerIdx = payload.Fills[0].MakerIdx
	raw := mustMarshalPreparedPayload(t, payload)

	if _, err := decodePreparedPayload(raw, BuildConfig{ProgramID: programID}); err == nil {
		t.Fatalf("expected decodePreparedPayload to reject maker=taker fill")
	}
}

func TestDecodePreparedPayloadRejectsUnusedUniqueUsers(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	payload := mustPreparedPayloadFromSingleFillBatch(t, programID)
	payload.UniqueUsers = append(payload.UniqueUsers, solana.NewWallet().PublicKey().String())
	raw := mustMarshalPreparedPayload(t, payload)

	if _, err := decodePreparedPayload(raw, BuildConfig{ProgramID: programID}); err == nil {
		t.Fatalf("expected decodePreparedPayload to reject unused unique users")
	}
}

func mustPreparedPayloadFromSingleFillBatch(t *testing.T, programID solana.PublicKey) preparedSubmissionPayload {
	t.Helper()
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
	raw, err := encodePreparedPayload(batch)
	if err != nil {
		t.Fatalf("encodePreparedPayload error: %v", err)
	}
	var payload preparedSubmissionPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("unmarshal prepared payload: %v", err)
	}
	return payload
}

func mustMarshalPreparedPayload(t *testing.T, payload preparedSubmissionPayload) []byte {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal prepared payload: %v", err)
	}
	return raw
}
