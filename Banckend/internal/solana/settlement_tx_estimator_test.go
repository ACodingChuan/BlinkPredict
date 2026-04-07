package solana_test

import (
	"encoding/base64"
	"encoding/hex"
	"testing"

	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/settlement"
	internalsolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
)

func TestSettlementTxEstimatorMatchesActualWireBytes(t *testing.T) {
	programID := solana.MustPublicKeyFromBase58("Buz3tgLcPxPDXGcQk38hzBvuUdb1yvvZjExC1g4tfibA")
	configPDA, err := internalsolana.DeriveConfigPDA(programID)
	if err != nil {
		t.Fatalf("derive config pda: %v", err)
	}

	tableID := solana.NewWallet().PublicKey()
	legacyCases := []struct {
		name          string
		warmOrderMask map[int]bool
		addressTables map[solana.PublicKey]solana.PublicKeySlice
	}{
		{
			name:          "legacy-mixed-cold",
			warmOrderMask: map[int]bool{0: false, 1: true, 2: false},
		},
		{
			name:          "v0-static-alt",
			warmOrderMask: map[int]bool{0: false, 1: true, 2: false},
			addressTables: map[solana.PublicKey]solana.PublicKeySlice{
				tableID: {
					programID,
					configPDA,
					solana.SysVarInstructionsPubkey,
					solana.SystemProgramID,
					solana.MustPublicKeyFromBase58("Ed25519SigVerify111111111111111111111111111"),
				},
			},
		},
		{
			name:          "legacy-all-warm",
			warmOrderMask: map[int]bool{0: true, 1: true, 2: true},
		},
	}

	for _, tc := range legacyCases {
		t.Run(tc.name, func(t *testing.T) {
			batch := makeSubmissionBatchForEstimatorTest(t, programID)
			for idx := range batch.Orders {
				batch.Orders[idx].Warm = tc.warmOrderMask[idx]
			}

			submitter := &settlement.Submitter{
				ProgramID:     programID,
				Relayer:       solana.NewWallet().PrivateKey,
				AddressTables: tc.addressTables,
			}
			tx, err := submitter.BuildTransactionWithBlockhash(batch, estimatorTestBlockhash())
			if err != nil {
				t.Fatalf("build tx: %v", err)
			}
			if _, _, err := submitter.SignTransaction(tx); err != nil {
				t.Fatalf("sign tx: %v", err)
			}
			wireBytes, err := submitter.TransactionWireBytes(tx)
			if err != nil {
				t.Fatalf("measure tx bytes: %v", err)
			}

			estimator := internalsolana.NewSettlementTxEstimator(programID, tc.addressTables)
			estimate, err := estimator.Estimate(settlementShapeForEstimatorTest(batch))
			if err != nil {
				t.Fatalf("estimate tx bytes: %v", err)
			}

			if wireBytes != estimate.TransactionBytes {
				t.Fatalf("wire bytes mismatch: actual=%d estimated=%d", wireBytes, estimate.TransactionBytes)
			}
			if tx.Message.IsVersioned() != estimate.Versioned {
				t.Fatalf("versioned mismatch: actual=%v estimated=%v", tx.Message.IsVersioned(), estimate.Versioned)
			}
			if len(tx.Message.GetAddressTableLookups()) != estimate.LookupTables {
				t.Fatalf("lookup table count mismatch: actual=%d estimated=%d", len(tx.Message.GetAddressTableLookups()), estimate.LookupTables)
			}
			if tx.Message.NumLookups() != estimate.LookupAccounts {
				t.Fatalf("lookup account count mismatch: actual=%d estimated=%d", tx.Message.NumLookups(), estimate.LookupAccounts)
			}
		})
	}
}

func TestSettlementTxEstimatorRejectsInvalidShapes(t *testing.T) {
	estimator := internalsolana.NewSettlementTxEstimator(solana.PublicKey{}, nil)
	cases := []internalsolana.SettlementTxShape{
		{UniqueUsers: 256, Orders: 1, ColdOrders: 1, Fills: 1},
		{UniqueUsers: 1, Orders: 256, ColdOrders: 1, Fills: 1},
		{UniqueUsers: 1, Orders: 1, ColdOrders: 256, Fills: 1},
		{UniqueUsers: 0, Orders: 1, ColdOrders: 1, Fills: 1},
		{UniqueUsers: 1, Orders: 0, ColdOrders: 0, Fills: 1},
		{UniqueUsers: 1, Orders: 1, ColdOrders: 2, Fills: 1},
	}

	for _, shape := range cases {
		if _, err := estimator.Estimate(shape); err == nil {
			t.Fatalf("expected invalid shape to fail: %+v", shape)
		}
	}
}

func makeSubmissionBatchForEstimatorTest(t *testing.T, programID solana.PublicKey) settlement.SubmissionBatch {
	t.Helper()

	market := solana.NewWallet().PublicKey()
	users := []solana.PublicKey{
		solana.NewWallet().PublicKey(),
		solana.NewWallet().PublicKey(),
		solana.NewWallet().PublicKey(),
	}
	event := matching.MatchBatchEvent{
		MarketID:  77,
		MarketPDA: market.String(),
		Orders: []matching.MatchedOrder{
			makeMatchedOrderForEstimatorTest(programID, market, users[0], 0, 101, settlement.SideBuy, settlement.OutcomeYes),
			makeMatchedOrderForEstimatorTest(programID, market, users[1], 1, 102, settlement.SideSell, settlement.OutcomeYes),
			makeMatchedOrderForEstimatorTest(programID, market, users[2], 2, 103, settlement.SideBuy, settlement.OutcomeNo),
		},
		Fills: []matching.MatchFill{
			{MakerOrderIndex: 0, TakerOrderIndex: 1, FillAmount: 25, FillPrice: 60},
			{MakerOrderIndex: 2, TakerOrderIndex: 1, FillAmount: 15, FillPrice: 55},
		},
	}

	batch, err := settlement.BuildSubmissionBatch(event, settlement.BuildConfig{ProgramID: programID})
	if err != nil {
		t.Fatalf("build submission batch: %v", err)
	}
	return batch
}

func makeMatchedOrderForEstimatorTest(
	programID solana.PublicKey,
	market solana.PublicKey,
	user solana.PublicKey,
	idx uint16,
	orderID uint64,
	side settlement.Side,
	outcome settlement.Outcome,
) matching.MatchedOrder {
	intent := settlement.OrderIntentV1{
		ProgramID:   programID,
		Market:      market,
		User:        user,
		Nonce:       orderID,
		Side:        side,
		Outcome:     outcome,
		OrderType:   settlement.OrderTypeLimit,
		LimitPrice:  60,
		TotalAmount: 500,
		ExpiryTs:    123,
	}
	return matching.MatchedOrder{
		OrderIndex: idx,
		OrderID:    orderID,
		Execution: matching.ExecutionSnapshot{
			OrderID:           orderID,
			WalletAddress:     user.String(),
			OriginalAction:    map[settlement.Side]string{settlement.SideBuy: "buy", settlement.SideSell: "sell"}[side],
			OriginalOutcome:   map[settlement.Outcome]string{settlement.OutcomeYes: "yes", settlement.OutcomeNo: "no"}[outcome],
			OriginalPriceTick: 60,
			OrderType:         "limit",
			ExpireTime:        123,
			Nonce:             orderID,
		},
		Settlement: matching.SettlementPayload{
			IntentBytesHex: hex.EncodeToString(intent.Serialize()),
			Signature:      base64.StdEncoding.EncodeToString(make([]byte, 64)),
		},
	}
}

func settlementShapeForEstimatorTest(batch settlement.SubmissionBatch) internalsolana.SettlementTxShape {
	coldOrders := 0
	for _, order := range batch.Orders {
		if !order.Warm {
			coldOrders++
		}
	}
	return internalsolana.SettlementTxShape{
		UniqueUsers: len(batch.UniqueUsers),
		Orders:      len(batch.Orders),
		ColdOrders:  coldOrders,
		Fills:       len(batch.Fills),
	}
}

func estimatorTestBlockhash() solana.Hash {
	var out solana.Hash
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}
