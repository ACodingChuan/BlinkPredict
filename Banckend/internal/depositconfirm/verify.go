package depositconfirm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"blinkpredict/banckend/internal/config"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type VerifiedDeposit struct {
	Signature     string
	WalletAddress string
	AmountUnits   uint64
	Slot          uint64
}

type retryableVerifyError struct {
	err error
}

func (e *retryableVerifyError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *retryableVerifyError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func markVerifyRetryable(err error) error {
	if err == nil {
		return nil
	}
	return &retryableVerifyError{err: err}
}

func isRetryableVerifyError(err error) bool {
	var tagged *retryableVerifyError
	return errors.As(err, &tagged)
}

func VerifyDepositTransaction(ctx context.Context, rpcClient *rpc.Client, cfg config.Config, expected Submission) (VerifiedDeposit, error) {
	if rpcClient == nil {
		return VerifiedDeposit{}, markVerifyRetryable(fmt.Errorf("rpc client is not configured"))
	}
	signature, err := solana.SignatureFromBase58(expected.Signature)
	if err != nil {
		return VerifiedDeposit{}, fmt.Errorf("invalid signature: %w", err)
	}
	programID, err := solana.PublicKeyFromBase58(strings.TrimSpace(cfg.ProgramID))
	if err != nil {
		return VerifiedDeposit{}, markVerifyRetryable(fmt.Errorf("invalid program id config: %w", err))
	}
	globalVault, err := solana.PublicKeyFromBase58(strings.TrimSpace(cfg.GlobalVault))
	if err != nil {
		return VerifiedDeposit{}, markVerifyRetryable(fmt.Errorf("invalid global vault config: %w", err))
	}
	mint, err := solana.PublicKeyFromBase58(strings.TrimSpace(cfg.VUSDCMint))
	if err != nil {
		return VerifiedDeposit{}, markVerifyRetryable(fmt.Errorf("invalid vusdc mint config: %w", err))
	}
	wallet, err := solana.PublicKeyFromBase58(strings.TrimSpace(expected.WalletAddress))
	if err != nil {
		return VerifiedDeposit{}, fmt.Errorf("invalid wallet address: %w", err)
	}
	maxVersion := uint64(0)
	out, err := rpcClient.GetParsedTransaction(ctx, signature, &rpc.GetParsedTransactionOpts{
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &maxVersion,
	})
	if err != nil {
		return VerifiedDeposit{}, markVerifyRetryable(fmt.Errorf("get parsed transaction: %w", err))
	}
	if out.Meta == nil || out.Meta.Err != nil {
		return VerifiedDeposit{}, fmt.Errorf("transaction failed or missing meta")
	}
	if out.Transaction == nil {
		return VerifiedDeposit{}, fmt.Errorf("transaction payload missing")
	}

	discriminator := anchorDiscriminator("deposit")
	for _, ix := range out.Transaction.Message.Instructions {
		if ix == nil {
			continue
		}
		if !ix.ProgramId.Equals(programID) {
			continue
		}
		if len(ix.Data) < 16 {
			continue
		}
		if string(ix.Data[:8]) != string(discriminator[:]) {
			continue
		}
		amount := binary.LittleEndian.Uint64(ix.Data[8:16])
		if amount != expected.AmountUnits {
			return VerifiedDeposit{}, fmt.Errorf("deposit amount mismatch: expected=%d got=%d", expected.AmountUnits, amount)
		}
		if len(ix.Accounts) < 8 {
			return VerifiedDeposit{}, fmt.Errorf("deposit instruction accounts layout is invalid")
		}
		if !ix.Accounts[0].Equals(wallet) {
			return VerifiedDeposit{}, fmt.Errorf("deposit wallet mismatch")
		}
		if !ix.Accounts[4].Equals(globalVault) {
			return VerifiedDeposit{}, fmt.Errorf("deposit global vault mismatch")
		}
		if !ix.Accounts[6].Equals(mint) {
			return VerifiedDeposit{}, fmt.Errorf("deposit mint mismatch")
		}
		return VerifiedDeposit{
			Signature:     expected.Signature,
			WalletAddress: expected.WalletAddress,
			AmountUnits:   expected.AmountUnits,
			Slot:          out.Slot,
		}, nil
	}
	return VerifiedDeposit{}, fmt.Errorf("deposit instruction not found in transaction")
}

func anchorDiscriminator(name string) [8]byte {
	hash := sha256.Sum256([]byte("global:" + name))
	var out [8]byte
	copy(out[:], hash[:8])
	return out
}
