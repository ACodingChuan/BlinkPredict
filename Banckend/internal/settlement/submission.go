package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"

	"blinkpredict/banckend/internal/matching"
	internalsolana "blinkpredict/banckend/internal/solana"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type SubmissionOrder struct {
	Intent     OrderIntentV1
	Signature  []byte
	RawIntent  []byte
	OrderIndex uint16
}

type FillIndexPair struct {
	MakerIdx   uint16
	TakerIdx   uint16
	FillAmount uint64
	FillPrice  uint64
}

type SubmissionBatch struct {
	MarketID    uint64
	MarketPDA   solana.PublicKey
	Orders      []SubmissionOrder
	Fills       []FillIndexPair
	UniqueUsers []solana.PublicKey
}

type BuildConfig struct {
	ProgramID solana.PublicKey
}

type TransactionSender interface {
	GetLatestBlockhash(ctx context.Context, commitment rpc.CommitmentType) (*rpc.GetLatestBlockhashResult, error)
	SendTransactionWithOpts(
		ctx context.Context,
		transaction *solana.Transaction,
		opts rpc.TransactionOpts,
	) (solana.Signature, error)
}

type Submitter struct {
	ProgramID solana.PublicKey
	Relayer   solana.PrivateKey
	RPC       TransactionSender
}

var ErrNoSettlementWork = errors.New("match batch contains no fills")

func BuildSubmissionBatch(event matching.MatchBatchEventV2, cfg BuildConfig) (SubmissionBatch, error) {
	if len(event.Fills) == 0 {
		return SubmissionBatch{}, ErrNoSettlementWork
	}
	if len(event.Orders) == 0 {
		return SubmissionBatch{}, fmt.Errorf("match batch has fills but no orders")
	}
	marketPDA, err := solana.PublicKeyFromBase58(event.MarketPDA)
	if err != nil {
		return SubmissionBatch{}, fmt.Errorf("parse market pda: %w", err)
	}
	sortedOrders := append([]matching.MatchedOrderV2(nil), event.Orders...)
	sort.Slice(sortedOrders, func(i, j int) bool {
		return sortedOrders[i].OrderIndex < sortedOrders[j].OrderIndex
	})

	orders := make([]SubmissionOrder, 0, len(sortedOrders))
	uniqueUsers := make([]solana.PublicKey, 0, len(event.Orders))
	for idx, order := range sortedOrders {
		if int(order.OrderIndex) != idx {
			return SubmissionBatch{}, fmt.Errorf("orders must be contiguous by order_index: got %d at position %d", order.OrderIndex, idx)
		}
		intent, rawIntent, sig, err := parseMatchedOrder(order)
		if err != nil {
			return SubmissionBatch{}, err
		}
		if !intent.ProgramID.Equals(cfg.ProgramID) {
			return SubmissionBatch{}, fmt.Errorf("intent program id mismatch for order %d", order.OrderID)
		}
		if err := validateIntentAgainstExecution(intent, marketPDA, order.Execution); err != nil {
			return SubmissionBatch{}, err
		}
		orders = append(orders, SubmissionOrder{Intent: intent, Signature: sig, RawIntent: rawIntent, OrderIndex: order.OrderIndex})
		if !containsPubkey(uniqueUsers, intent.User) {
			uniqueUsers = append(uniqueUsers, intent.User)
		}
	}
	fills := make([]FillIndexPair, 0, len(event.Fills))
	for _, fill := range event.Fills {
		if fill.FillAmount == 0 {
			return SubmissionBatch{}, fmt.Errorf("fill %d has zero fill_amount", fill.FillIndex)
		}
		if fill.FillPrice == 0 || fill.FillPrice >= 100 {
			return SubmissionBatch{}, fmt.Errorf("fill %d has invalid fill_price %d", fill.FillIndex, fill.FillPrice)
		}
		if int(fill.MakerOrderIndex) >= len(orders) || int(fill.TakerOrderIndex) >= len(orders) {
			return SubmissionBatch{}, fmt.Errorf("fill %d references missing order index", fill.FillIndex)
		}
		if fill.MakerOrderIndex == fill.TakerOrderIndex {
			return SubmissionBatch{}, fmt.Errorf("fill %d cannot reference the same maker and taker order", fill.FillIndex)
		}
		fills = append(fills, FillIndexPair{MakerIdx: fill.MakerOrderIndex, TakerIdx: fill.TakerOrderIndex, FillAmount: fill.FillAmount, FillPrice: fill.FillPrice})
	}
	return SubmissionBatch{MarketID: event.MarketID, MarketPDA: marketPDA, Orders: orders, Fills: fills, UniqueUsers: uniqueUsers}, nil
}

func (s *Submitter) BuildInstructions(batch SubmissionBatch, initPlan UserPositionInitPlan) ([]solana.Instruction, error) {
	configPDA, err := internalsolana.DeriveConfigPDA(s.ProgramID)
	if err != nil {
		return nil, err
	}
	instructions := make([]solana.Instruction, 0, len(batch.Orders)+len(initPlan.NeedInit)+1)
	for _, order := range batch.Orders {
		instructions = append(instructions, buildEd25519Instruction(order.Intent.User, order.Intent.SignableMessage(), order.Signature))
	}
	for _, entry := range initPlan.NeedInit {
		instructions = append(instructions, buildInitUserPositionInstruction(s.ProgramID, s.Relayer.PublicKey(), configPDA, entry))
	}
	settleIx, err := s.buildSettleMatchBatchInstruction(batch, configPDA)
	if err != nil {
		return nil, err
	}
	instructions = append(instructions, settleIx)
	return instructions, nil
}

func (s *Submitter) buildSettleMatchBatchInstruction(batch SubmissionBatch, configPDA solana.PublicKey) (solana.Instruction, error) {
	data, err := encodeSettleMatchBatchArgs(batch)
	if err != nil {
		return nil, err
	}
	accounts := []*solana.AccountMeta{
		solana.NewAccountMeta(s.Relayer.PublicKey(), true, true),
		solana.NewAccountMeta(configPDA, false, false),
		solana.NewAccountMeta(batch.MarketPDA, false, true),
		solana.NewAccountMeta(solana.SysVarInstructionsPubkey, false, false),
		solana.NewAccountMeta(solana.SystemProgramID, false, false),
	}
	for _, user := range batch.UniqueUsers {
		ledgerPDA, err := internalsolana.DeriveUserLedgerPDA(s.ProgramID, user)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(ledgerPDA, false, true))
	}
	for _, user := range batch.UniqueUsers {
		positionPDA, err := internalsolana.DeriveUserPositionPDA(s.ProgramID, user, batch.MarketPDA)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(positionPDA, false, true))
	}
	for _, order := range batch.Orders {
		orderStatePDA, err := internalsolana.DeriveOrderStatePDA(s.ProgramID, order.Intent.User, batch.MarketPDA, order.Intent.Nonce)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(orderStatePDA, false, true))
	}
	return solana.NewInstruction(s.ProgramID, accounts, data), nil
}

func (s *Submitter) BuildTransaction(ctx context.Context, batch SubmissionBatch, initPlan UserPositionInitPlan) (*solana.Transaction, error) {
	instructions, err := s.BuildInstructions(batch, initPlan)
	if err != nil {
		return nil, err
	}
	latest, err := s.RPC.GetLatestBlockhash(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return nil, fmt.Errorf("get latest blockhash: %w", err)
	}
	return solana.NewTransaction(instructions, latest.Value.Blockhash, solana.TransactionPayer(s.Relayer.PublicKey()))
}

func (s *Submitter) Submit(ctx context.Context, tx *solana.Transaction) (solana.Signature, error) {
	_, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.Relayer.PublicKey()) {
			return &s.Relayer
		}
		return nil
	})
	if err != nil {
		return solana.Signature{}, fmt.Errorf("sign settlement tx: %w", err)
	}
	return s.RPC.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: false, PreflightCommitment: rpc.CommitmentConfirmed})
}

func parseMatchedOrder(order matching.MatchedOrderV2) (OrderIntentV1, []byte, []byte, error) {
	rawIntent, err := decodeHex(order.Settlement.IntentBytesHex)
	if err != nil {
		return OrderIntentV1{}, nil, nil, fmt.Errorf("decode intent hex for order %d: %w", order.OrderID, err)
	}
	intent, err := ParseOrderIntentV1(rawIntent)
	if err != nil {
		return OrderIntentV1{}, nil, nil, fmt.Errorf("parse intent for order %d: %w", order.OrderID, err)
	}
	signature, err := base64.StdEncoding.DecodeString(order.Settlement.Signature)
	if err != nil {
		signature, err = base64.RawStdEncoding.DecodeString(order.Settlement.Signature)
		if err != nil {
			return OrderIntentV1{}, nil, nil, fmt.Errorf("decode signature for order %d: %w", order.OrderID, err)
		}
	}
	return intent, rawIntent, signature, nil
}

func validateIntentAgainstExecution(intent OrderIntentV1, marketPDA solana.PublicKey, exec matching.ExecutionSnapshotV2) error {
	if !intent.Market.Equals(marketPDA) {
		return fmt.Errorf("intent market mismatch for order %d", exec.OrderID)
	}
	user, err := solana.PublicKeyFromBase58(exec.WalletAddress)
	if err != nil {
		return fmt.Errorf("parse execution wallet: %w", err)
	}
	if !intent.User.Equals(user) {
		return fmt.Errorf("intent user mismatch for order %d", exec.OrderID)
	}
	if intent.Nonce != exec.Nonce {
		return fmt.Errorf("intent nonce mismatch for order %d", exec.OrderID)
	}
	if intent.ExpiryTs != exec.ExpireTime {
		return fmt.Errorf("intent expiry mismatch for order %d", exec.OrderID)
	}
	if intent.LimitPrice != uint64(exec.OriginalPriceTick) {
		return fmt.Errorf("intent limit price mismatch for order %d", exec.OrderID)
	}
	if intent.TotalAmount == 0 {
		return fmt.Errorf("intent total amount is zero for order %d", exec.OrderID)
	}
	if err := compareSide(intent.Side, exec.OriginalAction); err != nil {
		return fmt.Errorf("order %d: %w", exec.OrderID, err)
	}
	if err := compareOutcome(intent.Outcome, exec.OriginalOutcome); err != nil {
		return fmt.Errorf("order %d: %w", exec.OrderID, err)
	}
	if err := compareOrderType(intent.OrderType, exec.OrderType); err != nil {
		return fmt.Errorf("order %d: %w", exec.OrderID, err)
	}
	return nil
}

func compareSide(side Side, raw string) error {
	switch raw {
	case "buy":
		if side != SideBuy {
			return fmt.Errorf("side mismatch")
		}
	case "sell":
		if side != SideSell {
			return fmt.Errorf("side mismatch")
		}
	default:
		return fmt.Errorf("unknown side %q", raw)
	}
	return nil
}

func compareOutcome(outcome Outcome, raw string) error {
	switch raw {
	case "yes":
		if outcome != OutcomeYes {
			return fmt.Errorf("outcome mismatch")
		}
	case "no":
		if outcome != OutcomeNo {
			return fmt.Errorf("outcome mismatch")
		}
	default:
		return fmt.Errorf("unknown outcome %q", raw)
	}
	return nil
}

func compareOrderType(orderType OrderType, raw string) error {
	switch raw {
	case "limit":
		if orderType != OrderTypeLimit {
			return fmt.Errorf("order type mismatch")
		}
	case "market":
		if orderType != OrderTypeMarket {
			return fmt.Errorf("order type mismatch")
		}
	default:
		return fmt.Errorf("unknown order type %q", raw)
	}
	return nil
}

func encodeSettleMatchBatchArgs(batch SubmissionBatch) ([]byte, error) {
	args := make([]byte, 0, 1024)
	args = append(args, anchorDiscriminator("global:settle_match_batch")...)
	args = append(args, u32le(uint32(len(batch.Orders)))...)
	for _, order := range batch.Orders {
		args = append(args, order.Intent.Serialize()...)
	}
	args = append(args, u32le(uint32(len(batch.Fills)))...)
	for _, fill := range batch.Fills {
		args = append(args, u16le(fill.MakerIdx)...)
		args = append(args, u16le(fill.TakerIdx)...)
		args = append(args, u64le(fill.FillAmount)...)
		args = append(args, u64le(fill.FillPrice)...)
	}
	return args, nil
}

func buildInitUserPositionInstruction(programID, relayer, configPDA solana.PublicKey, entry UserPositionPlanEntry) solana.Instruction {
	data := anchorDiscriminator("global:init_user_position")
	accounts := []*solana.AccountMeta{
		solana.NewAccountMeta(relayer, true, true),
		solana.NewAccountMeta(configPDA, false, false),
		solana.NewAccountMeta(entry.UserPublicKey, false, false),
		solana.NewAccountMeta(entry.MarketPDA, false, false),
		solana.NewAccountMeta(entry.PositionPDA, false, true),
		solana.NewAccountMeta(solana.SystemProgramID, false, false),
	}
	return solana.NewInstruction(programID, accounts, data)
}

func buildEd25519Instruction(pubkey solana.PublicKey, message []byte, signature []byte) solana.Instruction {
	const off = 16
	msgOff := off + 64 + 32
	data := make([]byte, 0, msgOff+len(message))
	data = append(data, 1, 0)
	data = append(data, u16le(off)...)
	data = append(data, []byte{0xff, 0xff}...)
	data = append(data, u16le(off+64)...)
	data = append(data, []byte{0xff, 0xff}...)
	data = append(data, u16le(uint16(msgOff))...)
	data = append(data, u16le(uint16(len(message)))...)
	data = append(data, []byte{0xff, 0xff}...)
	data = append(data, signature...)
	data = append(data, pubkey.Bytes()...)
	data = append(data, message...)
	return solana.NewInstruction(solana.MustPublicKeyFromBase58("Ed25519SigVerify111111111111111111111111111"), nil, data)
}

func anchorDiscriminator(name string) []byte {
	sum := sha256.Sum256([]byte(name))
	return sum[:8]
}

func decodeHex(raw string) ([]byte, error) {
	return hex.DecodeString(raw)
}

func u16le(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }
func u32le(v uint32) []byte { return []byte{byte(v), byte(v >> 8), byte(v >> 16), byte(v >> 24)} }

func containsPubkey(list []solana.PublicKey, key solana.PublicKey) bool {
	for _, item := range list {
		if item.Equals(key) {
			return true
		}
	}
	return false
}

func SortUsersForAccounts(users []solana.PublicKey) {
	sort.Slice(users, func(i, j int) bool { return users[i].String() < users[j].String() })
}
