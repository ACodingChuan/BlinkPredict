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

func DeriveAddresses(programID solana.PublicKey, marketID uint64) (DerivedAddresses, error) {
	seed := make([]byte, 8)
	binary.LittleEndian.PutUint64(seed, marketID)

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
