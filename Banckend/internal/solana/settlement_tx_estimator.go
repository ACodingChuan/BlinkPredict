package solana

import (
	"encoding/binary"
	"fmt"
	"sync"

	gsolana "github.com/gagliardetto/solana-go"
)

const (
	DefaultSettlementMaxTxBytes  = 1232
	settlementSignableMessageLen = 47
)

// SettlementTxShape contains only the dimensions that change the serialized
// transaction size. It intentionally does not model CU-only dimensions.
type SettlementTxShape struct {
	UniqueUsers int
	Orders      int
	ColdOrders  int
	Fills       int
}

// SettlementTxEstimate is a byte-level estimate of the fully built settlement
// transaction. TransactionBytes is the raw wire size after message assembly,
// recent blockhash insertion, payer signature, and optional v0 lookup encoding.
type SettlementTxEstimate struct {
	TransactionBytes int
	Versioned        bool
	LookupTables     int
	LookupAccounts   int
}

type settlementTxCacheKey struct {
	UniqueUsers int
	Orders      int
	ColdOrders  int
	Fills       int
}

type SettlementTxEstimator struct {
	programID     gsolana.PublicKey
	configPDA     gsolana.PublicKey
	addressTables map[gsolana.PublicKey]gsolana.PublicKeySlice
	payer         gsolana.PrivateKey
	cache         sync.Map
}

func NewSettlementTxEstimator(programID gsolana.PublicKey, addressTables map[gsolana.PublicKey]gsolana.PublicKeySlice) *SettlementTxEstimator {
	if programID.IsZero() {
		programID = estimatorDummyPubkey(0x10, 0)
	}
	configPDA, err := DeriveConfigPDA(programID)
	if err != nil {
		configPDA = estimatorDummyPubkey(0x11, 0)
	}
	return &SettlementTxEstimator{
		programID:     programID,
		configPDA:     configPDA,
		addressTables: CopyAddressTables(addressTables),
		payer:         gsolana.NewWallet().PrivateKey,
	}
}

// Estimate returns the raw serialized transaction size for one settlement batch
// shape. It builds a synthetic transaction and marshals it, so the estimate
// includes Solana submission overhead such as signatures, message header,
// recent blockhash, instruction envelopes, and optional v0/ALT lookup bytes.
func (e *SettlementTxEstimator) Estimate(shape SettlementTxShape) (SettlementTxEstimate, error) {
	if err := validateSettlementTxShape(shape); err != nil {
		return SettlementTxEstimate{}, err
	}
	key := settlementTxCacheKey(shape)
	if cached, ok := e.cache.Load(key); ok {
		return cached.(SettlementTxEstimate), nil
	}
	tx, err := e.buildSyntheticTransaction(shape)
	if err != nil {
		return SettlementTxEstimate{}, err
	}
	raw, err := tx.MarshalBinary()
	if err != nil {
		return SettlementTxEstimate{}, fmt.Errorf("marshal synthetic settlement tx: %w", err)
	}
	estimate := SettlementTxEstimate{
		TransactionBytes: len(raw),
		Versioned:        tx.Message.IsVersioned(),
		LookupTables:     len(tx.Message.GetAddressTableLookups()),
		LookupAccounts:   tx.Message.NumLookups(),
	}
	e.cache.Store(key, estimate)
	return estimate, nil
}

func (e *SettlementTxEstimator) MaxFillsWithinLimit(base SettlementTxShape, maxTxBytes int, ceiling int) (int, SettlementTxEstimate, error) {
	if ceiling < 0 {
		ceiling = 0
	}
	if maxTxBytes <= 0 {
		maxTxBytes = DefaultSettlementMaxTxBytes
	}
	base.Fills = 0
	bestFills := 0
	var bestEstimate SettlementTxEstimate
	low, high := 0, ceiling
	for low <= high {
		mid := (low + high) / 2
		shape := base
		shape.Fills = mid
		estimate, err := e.Estimate(shape)
		if err != nil {
			return 0, SettlementTxEstimate{}, err
		}
		if estimate.TransactionBytes <= maxTxBytes {
			bestFills = mid
			bestEstimate = estimate
			low = mid + 1
			continue
		}
		high = mid - 1
	}
	return bestFills, bestEstimate, nil
}

func validateSettlementTxShape(shape SettlementTxShape) error {
	switch {
	case shape.UniqueUsers < 0:
		return fmt.Errorf("unique users must be >= 0")
	case shape.UniqueUsers > 255:
		return fmt.Errorf("unique users must be <= 255")
	case shape.Orders < 0:
		return fmt.Errorf("orders must be >= 0")
	case shape.Orders > 255:
		return fmt.Errorf("orders must be <= 255")
	case shape.ColdOrders < 0:
		return fmt.Errorf("cold orders must be >= 0")
	case shape.ColdOrders > 255:
		return fmt.Errorf("cold orders must be <= 255")
	case shape.Fills < 0:
		return fmt.Errorf("fills must be >= 0")
	case shape.Orders > 0 && shape.UniqueUsers == 0:
		return fmt.Errorf("orders require at least one unique user")
	case shape.ColdOrders > shape.Orders:
		return fmt.Errorf("cold orders %d exceed orders %d", shape.ColdOrders, shape.Orders)
	case shape.Fills > 0 && shape.Orders == 0:
		return fmt.Errorf("fills require at least one order")
	}
	return nil
}

func (e *SettlementTxEstimator) buildSyntheticTransaction(shape SettlementTxShape) (*gsolana.Transaction, error) {
	instructions := make([]gsolana.Instruction, 0, shape.ColdOrders+1)
	for i := 0; i < shape.ColdOrders; i++ {
		instructions = append(instructions, estimatorBuildEd25519Instruction(estimatorDummyPubkey(0x80, i), settlementSignableMessageLen))
	}
	instructions = append(instructions, e.buildSyntheticSettlementInstruction(shape))
	opts := []gsolana.TransactionOption{gsolana.TransactionPayer(e.payer.PublicKey())}
	if len(e.addressTables) > 0 {
		opts = append(opts, gsolana.TransactionAddressTables(CopyAddressTables(e.addressTables)))
	}
	tx, err := gsolana.NewTransaction(instructions, estimatorDummyHash(), opts...)
	if err != nil {
		return nil, fmt.Errorf("build synthetic settlement tx: %w", err)
	}
	_, err = tx.Sign(func(key gsolana.PublicKey) *gsolana.PrivateKey {
		if key.Equals(e.payer.PublicKey()) {
			return &e.payer
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("sign synthetic settlement tx: %w", err)
	}
	return tx, nil
}

func (e *SettlementTxEstimator) buildSyntheticSettlementInstruction(shape SettlementTxShape) gsolana.Instruction {
	accounts := make([]*gsolana.AccountMeta, 0, 5+(shape.UniqueUsers*2)+shape.Orders)
	accounts = append(accounts,
		gsolana.NewAccountMeta(e.payer.PublicKey(), true, true),
		gsolana.NewAccountMeta(e.configPDA, false, false),
		gsolana.NewAccountMeta(estimatorDummyPubkey(0x12, 0), true, false),
		gsolana.NewAccountMeta(gsolana.SysVarInstructionsPubkey, false, false),
		gsolana.NewAccountMeta(gsolana.SystemProgramID, false, false),
	)
	for i := 0; i < shape.UniqueUsers; i++ {
		accounts = append(accounts, gsolana.NewAccountMeta(estimatorDummyPubkey(0x20, i), true, false))
	}
	for i := 0; i < shape.UniqueUsers; i++ {
		accounts = append(accounts, gsolana.NewAccountMeta(estimatorDummyPubkey(0x30, i), true, false))
	}
	for i := 0; i < shape.Orders; i++ {
		accounts = append(accounts, gsolana.NewAccountMeta(estimatorDummyPubkey(0x40, i), true, false))
	}
	return gsolana.NewInstruction(e.programID, accounts, estimatorEncodeSettleArgs(shape))
}

func estimatorEncodeSettleArgs(shape SettlementTxShape) []byte {
	data := make([]byte, 0, 32+(shape.Orders*2)+(shape.ColdOrders*22)+(shape.Fills*7))
	data = append(data, estimatorDummyDiscriminator()...)
	data = append(data, byte(shape.UniqueUsers))
	data = append(data, estimatorU32LE(uint32(shape.Orders))...)
	for i := 0; i < shape.Orders; i++ {
		userIdx := byte(0)
		if shape.UniqueUsers > 0 {
			userIdx = byte(i % shape.UniqueUsers)
		}
		coldIdx := byte(0xff)
		if i < shape.ColdOrders {
			coldIdx = byte(i)
		}
		data = append(data, userIdx, coldIdx)
	}
	data = append(data, estimatorU32LE(uint32(shape.ColdOrders))...)
	for i := 0; i < shape.ColdOrders; i++ {
		data = append(data, estimatorU64LE(uint64(i+1))...)
		data = append(data, estimatorU64LE(uint64(1000+i))...)
		data = append(data, estimatorU32LE(uint32(1_900_000_000+i))...)
		data = append(data, byte(50))
		data = append(data, byte(0))
	}
	data = append(data, estimatorU32LE(uint32(shape.Fills))...)
	for i := 0; i < shape.Fills; i++ {
		makerIdx := byte(0)
		takerIdx := byte(0)
		if shape.Orders > 0 {
			makerIdx = byte(i % shape.Orders)
			takerIdx = byte((i + 1) % shape.Orders)
		}
		data = append(data, makerIdx, takerIdx)
		data = append(data, estimatorU32LE(uint32(100+i))...)
		data = append(data, byte(50))
	}
	return data
}

func estimatorBuildEd25519Instruction(pubkey gsolana.PublicKey, messageLen int) gsolana.Instruction {
	const headerLen = 16
	msgOff := headerLen + 64 + 32
	data := make([]byte, 0, msgOff+messageLen)
	data = append(data, 1, 0)
	data = append(data, estimatorU16LE(headerLen)...)
	data = append(data, 0xff, 0xff)
	data = append(data, estimatorU16LE(headerLen+64)...)
	data = append(data, 0xff, 0xff)
	data = append(data, estimatorU16LE(msgOff)...)
	data = append(data, estimatorU16LE(messageLen)...)
	data = append(data, 0xff, 0xff)
	data = append(data, make([]byte, 64)...)
	data = append(data, pubkey[:]...)
	data = append(data, make([]byte, messageLen)...)
	return gsolana.NewInstruction(gsolana.MustPublicKeyFromBase58("Ed25519SigVerify111111111111111111111111111"), nil, data)
}

func estimatorDummyPubkey(prefix byte, idx int) gsolana.PublicKey {
	var out gsolana.PublicKey
	out[0] = prefix
	binary.LittleEndian.PutUint32(out[1:5], uint32(idx))
	binary.LittleEndian.PutUint32(out[5:9], uint32(idx*17+3))
	out[31] = byte(idx % 251)
	return out
}

func estimatorDummyHash() gsolana.Hash {
	var out gsolana.Hash
	for i := range out {
		out[i] = byte(i + 1)
	}
	return out
}

func estimatorDummyDiscriminator() []byte {
	return []byte{0x7a, 0x11, 0x9c, 0x4f, 0x53, 0x22, 0x18, 0x91}
}

func estimatorU16LE(value int) []byte {
	out := make([]byte, 2)
	binary.LittleEndian.PutUint16(out, uint16(value))
	return out
}

func estimatorU32LE(value uint32) []byte {
	out := make([]byte, 4)
	binary.LittleEndian.PutUint32(out, value)
	return out
}

func estimatorU64LE(value uint64) []byte {
	out := make([]byte, 8)
	binary.LittleEndian.PutUint64(out, value)
	return out
}
