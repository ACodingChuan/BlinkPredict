package solana

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"

	solana "github.com/gagliardetto/solana-go"
)

type DerivedAddresses struct {
	MarketPDA       solana.PublicKey
	CollateralVault solana.PublicKey
	YesMint         solana.PublicKey
	NoMint          solana.PublicKey
}

func u64LE(value uint64) []byte {
	seed := make([]byte, 8)
	binary.LittleEndian.PutUint64(seed, value)
	return seed
}

func DeriveMarketPDA(programID solana.PublicKey, marketID uint64) (solana.PublicKey, error) {
	seed := u64LE(marketID)
	market, _, err := solana.FindProgramAddress([][]byte{[]byte("market"), seed}, programID)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive market pda: %w", err)
	}
	return market, nil
}

func DeriveConfigPDA(programID solana.PublicKey) (solana.PublicKey, error) {
	config, _, err := solana.FindProgramAddress([][]byte{[]byte("config")}, programID)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive config pda: %w", err)
	}
	return config, nil
}

func DeriveUserLedgerPDA(programID, user solana.PublicKey) (solana.PublicKey, error) {
	ledger, _, err := solana.FindProgramAddress([][]byte{[]byte("user_ledger"), user.Bytes()}, programID)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive user ledger pda: %w", err)
	}
	return ledger, nil
}

func DeriveUserPositionPDA(programID, user, market solana.PublicKey) (solana.PublicKey, error) {
	position, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("position"), user.Bytes(), market.Bytes()},
		programID,
	)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive user position pda: %w", err)
	}
	return position, nil
}

func DeriveOrderStatePDA(programID, user, market solana.PublicKey, nonce uint64) (solana.PublicKey, error) {
	orderState, _, err := solana.FindProgramAddress(
		[][]byte{[]byte("order"), user.Bytes(), market.Bytes(), u64LE(nonce)},
		programID,
	)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("derive order state pda: %w", err)
	}
	return orderState, nil
}

func DeriveAddresses(programID solana.PublicKey, marketID uint64) (DerivedAddresses, error) {
	seed := u64LE(marketID)

	market, _, err := solana.FindProgramAddress([][]byte{[]byte("market"), seed}, programID)
	if err != nil {
		return DerivedAddresses{}, fmt.Errorf("derive market pda: %w", err)
	}
	vault, _, err := solana.FindProgramAddress([][]byte{[]byte("collateral_vault"), seed}, programID)
	if err != nil {
		return DerivedAddresses{}, fmt.Errorf("derive collateral vault: %w", err)
	}
	yesMint, _, err := solana.FindProgramAddress([][]byte{[]byte("yes_mint"), seed}, programID)
	if err != nil {
		return DerivedAddresses{}, fmt.Errorf("derive yes mint: %w", err)
	}
	noMint, _, err := solana.FindProgramAddress([][]byte{[]byte("no_mint"), seed}, programID)
	if err != nil {
		return DerivedAddresses{}, fmt.Errorf("derive no mint: %w", err)
	}

	return DerivedAddresses{MarketPDA: market, CollateralVault: vault, YesMint: yesMint, NoMint: noMint}, nil
}

func StableMarketID(input string) uint64 {
	hash := sha256.Sum256([]byte(input))
	return binary.LittleEndian.Uint64(hash[:8])
}
