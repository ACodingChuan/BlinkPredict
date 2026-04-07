package settlement

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"

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
	Warm       bool
}

type FillIndexPair struct {
	MakerIdx   uint8
	TakerIdx   uint8
	FillAmount uint32
	FillPrice  uint8
}

type OrderSlot struct {
	UserIdx        uint8
	ColdWitnessIdx uint8
}

type ColdOrderWitness struct {
	Nonce       uint64
	TotalAmount uint64
	ExpiryTs    uint32
	LimitPrice  uint8
	Flags       uint8
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
	ProgramID     solana.PublicKey
	Relayer       solana.PrivateKey
	RPC           TransactionSender
	AddressTables map[solana.PublicKey]solana.PublicKeySlice
}

var ErrNoSettlementWork = errors.New("match batch contains no fills")

func BuildSubmissionBatch(event matching.MatchBatchEvent, cfg BuildConfig) (SubmissionBatch, error) {
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
	if len(event.Orders) > 255 {
		return SubmissionBatch{}, fmt.Errorf("match batch has too many orders: %d", len(event.Orders))
	}
	sortedOrders := append([]matching.MatchedOrder(nil), event.Orders...)
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
	if len(uniqueUsers) > 255 {
		return SubmissionBatch{}, fmt.Errorf("match batch has too many unique users: %d", len(uniqueUsers))
	}
	fills := make([]FillIndexPair, 0, len(event.Fills))
	for _, fill := range event.Fills {
		if fill.FillAmount == 0 {
			return SubmissionBatch{}, fmt.Errorf("fill %d has zero fill_amount", fill.FillIndex)
		}
		if fill.FillAmount > uint64(^uint32(0)) {
			return SubmissionBatch{}, fmt.Errorf("fill %d exceeds u32 amount limit", fill.FillIndex)
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
		fills = append(fills, FillIndexPair{
			MakerIdx:   uint8(fill.MakerOrderIndex),
			TakerIdx:   uint8(fill.TakerOrderIndex),
			FillAmount: uint32(fill.FillAmount),
			FillPrice:  uint8(fill.FillPrice),
		})
	}
	return SubmissionBatch{MarketID: event.MarketID, MarketPDA: marketPDA, Orders: orders, Fills: fills, UniqueUsers: uniqueUsers}, nil
}

func (s *Submitter) BuildInstructions(batch SubmissionBatch) ([]solana.Instruction, error) {
	configPDA, err := internalsolana.DeriveConfigPDA(s.ProgramID)
	if err != nil {
		return nil, err
	}
	instructions := make([]solana.Instruction, 0, len(batch.Orders)+1)
	for _, order := range batch.Orders {
		if order.Warm {
			continue
		}
		if len(order.Signature) != 64 {
			return nil, fmt.Errorf("invalid signature length for order %d: got %d want 64", order.OrderIndex, len(order.Signature))
		}
		instructions = append(instructions, buildEd25519Instruction(order.Intent.User, order.Intent.SignableMessage(), order.Signature))
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
		solana.NewAccountMeta(batch.MarketPDA, true, false),
		solana.NewAccountMeta(solana.SysVarInstructionsPubkey, false, false),
		solana.NewAccountMeta(solana.SystemProgramID, false, false),
	}
	for _, user := range batch.UniqueUsers {
		ledgerPDA, err := internalsolana.DeriveUserLedgerPDA(s.ProgramID, user)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(ledgerPDA, true, false))
	}
	for _, user := range batch.UniqueUsers {
		positionPDA, err := internalsolana.DeriveUserPositionPDA(s.ProgramID, user, batch.MarketPDA)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(positionPDA, true, false))
	}
	for _, order := range batch.Orders {
		orderStatePDA, err := internalsolana.DeriveOrderStatePDA(s.ProgramID, order.Intent.User, batch.MarketPDA, order.Intent.Nonce)
		if err != nil {
			return nil, err
		}
		accounts = append(accounts, solana.NewAccountMeta(orderStatePDA, true, false))
	}
	return solana.NewInstruction(s.ProgramID, accounts, data), nil
}

func (s *Submitter) BuildTransaction(ctx context.Context, batch SubmissionBatch) (*solana.Transaction, error) {
	instructions, err := s.BuildInstructions(batch)
	if err != nil {
		return nil, err
	}
	latest, err := s.RPC.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, fmt.Errorf("get latest blockhash: %w", err)
	}
	return s.newTransactionWithBlockhash(instructions, latest.Value.Blockhash)
}

func (s *Submitter) BuildSignedTransaction(
	ctx context.Context,
	batch SubmissionBatch,
) (*solana.Transaction, solana.Signature, string, uint64, error) {
	instructions, err := s.BuildInstructions(batch)
	if err != nil {
		return nil, solana.Signature{}, "", 0, err
	}
	latest, err := s.RPC.GetLatestBlockhash(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return nil, solana.Signature{}, "", 0, fmt.Errorf("get latest blockhash: %w", err)
	}
	tx, err := s.newTransactionWithBlockhash(instructions, latest.Value.Blockhash)
	if err != nil {
		return nil, solana.Signature{}, "", 0, err
	}
	sig, rawBase64, err := s.SignTransaction(tx)
	if err != nil {
		return nil, solana.Signature{}, "", 0, err
	}
	return tx, sig, rawBase64, latest.Value.LastValidBlockHeight, nil
}

func (s *Submitter) BuildTransactionWithBlockhash(batch SubmissionBatch, blockhash solana.Hash) (*solana.Transaction, error) {
	instructions, err := s.BuildInstructions(batch)
	if err != nil {
		return nil, err
	}
	return s.newTransactionWithBlockhash(instructions, blockhash)
}

func (s *Submitter) newTransactionWithBlockhash(instructions []solana.Instruction, blockhash solana.Hash) (*solana.Transaction, error) {
	opts := []solana.TransactionOption{solana.TransactionPayer(s.Relayer.PublicKey())}
	if len(s.AddressTables) > 0 {
		opts = append(opts, solana.TransactionAddressTables(internalsolana.CopyAddressTables(s.AddressTables)))
	}
	return solana.NewTransaction(instructions, blockhash, opts...)
}

func (s *Submitter) SignTransaction(tx *solana.Transaction) (solana.Signature, string, error) {
	if tx == nil {
		return solana.Signature{}, "", fmt.Errorf("transaction is nil")
	}
	_, err := tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.Relayer.PublicKey()) {
			return &s.Relayer
		}
		return nil
	})
	if err != nil {
		return solana.Signature{}, "", fmt.Errorf("sign settlement tx: %w", err)
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return solana.Signature{}, "", fmt.Errorf("marshal signed transaction: %w", err)
	}
	if len(tx.Signatures) == 0 {
		return solana.Signature{}, "", fmt.Errorf("extract transaction signature: missing signed signatures")
	}
	return tx.Signatures[0], base64.StdEncoding.EncodeToString(raw), nil
}

func (s *Submitter) TransactionWireBytes(tx *solana.Transaction) (int, error) {
	if tx == nil {
		return 0, fmt.Errorf("transaction is nil")
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return 0, fmt.Errorf("marshal transaction bytes: %w", err)
	}
	return len(raw), nil
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
	return s.RPC.SendTransactionWithOpts(ctx, tx, rpc.TransactionOpts{SkipPreflight: true, PreflightCommitment: rpc.CommitmentProcessed})
}

func parseMatchedOrder(order matching.MatchedOrder) (OrderIntentV1, []byte, []byte, error) {
	rawIntent, err := decodeHex(order.Settlement.IntentBytesHex)
	if err != nil {
		return OrderIntentV1{}, nil, nil, fmt.Errorf("decode intent hex for order %d: %w", order.OrderID, err)
	}
	intent, err := ParseOrderIntentV1(rawIntent)
	if err != nil {
		return OrderIntentV1{}, nil, nil, fmt.Errorf("parse intent for order %d: %w", order.OrderID, err)
	}
	signature, err := decodeSignature(order.Settlement.Signature)
	if err != nil {
		return OrderIntentV1{}, nil, nil, fmt.Errorf("decode signature for order %d: %w", order.OrderID, err)
	}
	return intent, rawIntent, signature, nil
}

func validateIntentAgainstExecution(intent OrderIntentV1, marketPDA solana.PublicKey, exec matching.ExecutionSnapshot) error {
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
	if exec.ExpireTime < 0 || exec.ExpireTime > int64(^uint32(0)) {
		return fmt.Errorf("execution expiry out of range for order %d", exec.OrderID)
	}
	if intent.ExpiryTs != uint32(exec.ExpireTime) {
		return fmt.Errorf("intent expiry mismatch for order %d", exec.OrderID)
	}
	if intent.LimitPrice == 0 || intent.LimitPrice >= 100 {
		return fmt.Errorf("intent limit price out of range for order %d", exec.OrderID)
	}
	if intent.LimitPrice != uint8(exec.OriginalPriceTick) {
		return fmt.Errorf("intent limit price mismatch for order %d", exec.OrderID)
	}
	if intent.TotalAmount == 0 {
		return fmt.Errorf("intent total amount is zero for order %d", exec.OrderID)
	}
	minNotional, err := minimumOrderNotional(intent)
	if err != nil {
		return fmt.Errorf("intent minimum notional overflow for order %d: %w", exec.OrderID, err)
	}
	if minNotional < 100 {
		return fmt.Errorf("intent minimum notional below 100 units for order %d", exec.OrderID)
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
	if len(batch.UniqueUsers) > 255 {
		return nil, fmt.Errorf("too many unique users: %d", len(batch.UniqueUsers))
	}
	userIndexByKey := make(map[string]uint8, len(batch.UniqueUsers))
	for idx, user := range batch.UniqueUsers {
		userIndexByKey[user.String()] = uint8(idx)
	}
	orderSlots := make([]OrderSlot, 0, len(batch.Orders))
	coldWitnesses := make([]ColdOrderWitness, 0, len(batch.Orders))
	args = append(args, byte(len(batch.UniqueUsers)))
	args = append(args, u32le(uint32(len(batch.Orders)))...)
	for _, order := range batch.Orders {
		userIdx, ok := userIndexByKey[order.Intent.User.String()]
		if !ok {
			return nil, fmt.Errorf("missing user index for order %d", order.OrderIndex)
		}
		slot := OrderSlot{UserIdx: userIdx, ColdWitnessIdx: 0xff}
		if !order.Warm {
			if len(coldWitnesses) >= 255 {
				return nil, fmt.Errorf("too many cold witnesses")
			}
			slot.ColdWitnessIdx = uint8(len(coldWitnesses))
			coldWitnesses = append(coldWitnesses, ColdOrderWitness{
				Nonce:       order.Intent.Nonce,
				TotalAmount: order.Intent.TotalAmount,
				ExpiryTs:    order.Intent.ExpiryTs,
				LimitPrice:  order.Intent.LimitPrice,
				Flags:       order.Intent.Flags(),
			})
		}
		orderSlots = append(orderSlots, slot)
	}
	for _, slot := range orderSlots {
		args = append(args, slot.UserIdx, slot.ColdWitnessIdx)
	}
	args = append(args, u32le(uint32(len(coldWitnesses)))...)
	for _, witness := range coldWitnesses {
		args = append(args, u64le(witness.Nonce)...)
		args = append(args, u64le(witness.TotalAmount)...)
		args = append(args, u32le(witness.ExpiryTs)...)
		args = append(args, witness.LimitPrice)
		args = append(args, witness.Flags)
	}
	args = append(args, u32le(uint32(len(batch.Fills)))...)
	for _, fill := range batch.Fills {
		args = append(args, fill.MakerIdx, fill.TakerIdx)
		args = append(args, u32le(fill.FillAmount)...)
		args = append(args, fill.FillPrice)
	}
	return args, nil
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
	cleaned := strings.Join(strings.Fields(raw), "")
	return hex.DecodeString(cleaned)
}

func u16le(v uint16) []byte { return []byte{byte(v), byte(v >> 8)} }

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

func minimumOrderNotional(intent OrderIntentV1) (uint64, error) {
	if intent.OrderType == OrderTypeMarket && intent.Side == SideBuy {
		return intent.TotalAmount, nil
	}
	return ceilMulDiv(intent.TotalAmount, uint64(intent.LimitPrice), 100)
}

func ceilMulDiv(lhs, rhs, denominator uint64) (uint64, error) {
	if denominator == 0 {
		return 0, fmt.Errorf("denominator must be greater than zero")
	}
	product, overflow := mulUint64(lhs, rhs)
	if overflow {
		return 0, fmt.Errorf("multiply overflow")
	}
	quotient := product / denominator
	if product%denominator != 0 {
		if quotient == ^uint64(0) {
			return 0, fmt.Errorf("round up overflow")
		}
		quotient++
	}
	return quotient, nil
}

func mulUint64(lhs, rhs uint64) (uint64, bool) {
	if lhs == 0 || rhs == 0 {
		return 0, false
	}
	product := lhs * rhs
	return product, product/lhs != rhs
}
