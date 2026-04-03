package marketconfirm

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"strings"

	"blinkpredict/banckend/internal/config"
	internalsolana "blinkpredict/banckend/internal/solana"
	"blinkpredict/banckend/internal/protocol"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
)

type VerifiedMarket struct {
	protocol.MarketConfirmedEvent
}

func VerifyMarketCreateTransaction(ctx context.Context, rpcClient *rpc.Client, cfg config.Config, expected Submission) (VerifiedMarket, error) {
	if rpcClient == nil {
		return VerifiedMarket{}, fmt.Errorf("rpc client is not configured")
	}
	signature, err := solana.SignatureFromBase58(strings.TrimSpace(expected.Signature))
	if err != nil {
		return VerifiedMarket{}, fmt.Errorf("invalid signature: %w", err)
	}
	programID, err := solana.PublicKeyFromBase58(strings.TrimSpace(cfg.ProgramID))
	if err != nil {
		return VerifiedMarket{}, fmt.Errorf("invalid program id config: %w", err)
	}
	maxVersion := uint64(0)
	out, err := rpcClient.GetParsedTransaction(ctx, signature, &rpc.GetParsedTransactionOpts{
		Commitment:                     rpc.CommitmentConfirmed,
		MaxSupportedTransactionVersion: &maxVersion,
	})
	if err != nil {
		return VerifiedMarket{}, fmt.Errorf("get parsed transaction: %w", err)
	}
	if out == nil || out.Meta == nil || out.Meta.Err != nil || out.Transaction == nil {
		return VerifiedMarket{}, fmt.Errorf("transaction failed or missing meta")
	}

	discriminator := anchorDiscriminator("create_market")
	for _, ix := range out.Transaction.Message.Instructions {
		if ix == nil {
			continue
		}
		if !ix.ProgramId.Equals(programID) {
			continue
		}
		if len(ix.Data) < 8+8+96+8+8+8+1+32+1+8+4 {
			continue
		}
		if string(ix.Data[:8]) != string(discriminator[:]) {
			continue
		}
		if len(ix.Accounts) < 2 {
			return VerifiedMarket{}, fmt.Errorf("create_market instruction accounts layout is invalid")
		}
		marketID := binary.LittleEndian.Uint64(ix.Data[8:16])
		marketPDA := ix.Accounts[0]
		creator := ix.Accounts[1]
		derivedMarketPDA, err := internalsolana.DeriveMarketPDA(programID, marketID)
		if err != nil {
			return VerifiedMarket{}, fmt.Errorf("derive market pda: %w", err)
		}
		if !derivedMarketPDA.Equals(marketPDA) {
			return VerifiedMarket{}, fmt.Errorf("market pda mismatch")
		}
		metadataCID := decodeFixedString(ix.Data[16 : 16+96])
		closeTS := int64(binary.LittleEndian.Uint64(ix.Data[112:120]))
		resolveAfterTS := int64(binary.LittleEndian.Uint64(ix.Data[120:128]))
		claimDeadlineTS := int64(binary.LittleEndian.Uint64(ix.Data[128:136]))
		resolutionModeRaw := ix.Data[136]
		oracleFeedRaw := ix.Data[137:169]
		oracleConditionRaw := ix.Data[169]
		oracleTargetPrice := binary.LittleEndian.Uint64(ix.Data[170:178])
		oracleTargetExpo := int32(binary.LittleEndian.Uint32(ix.Data[178:182]))

		resolutionMode := normalizeResolutionMode(resolutionModeRaw)
		oracleFeed := ""
		if resolutionMode == "pyth" {
			oracleFeed = strings.ToLower(fmt.Sprintf("0x%x", oracleFeedRaw))
		}
		metadataDoc, metadataURL, err := fetchMetadata(ctx, metadataCID)
		if err != nil {
			return VerifiedMarket{}, fmt.Errorf("fetch metadata: %w", err)
		}
		verified := VerifiedMarket{MarketConfirmedEvent: protocol.MarketConfirmedEvent{
			Signature:           expected.Signature,
			Slot:                out.Slot,
			MarketID:            marketID,
			MarketPDA:           marketPDA.String(),
			Creator:             creator.String(),
			MetadataCID:         metadataCID,
			MetadataURL:         metadataURL,
			Title:               metadataDoc.Title,
			Description:         metadataDoc.Description,
			Category:            metadataDoc.Category,
			ImageURL:            metadataDoc.ImageURL,
			ResolutionMode:      resolutionMode,
			ResolutionAuthority: creator.String(),
			OracleFeed:          oracleFeed,
			OracleCondition:     normalizeOracleCondition(oracleConditionRaw),
			OracleTargetPrice:   oracleTargetPrice,
			OracleTargetExpo:    oracleTargetExpo,
			CloseTS:             closeTS,
			ResolveAfterTS:      resolveAfterTS,
			ClaimDeadlineTS:     claimDeadlineTS,
		}}
		if resolutionMode == "creator" {
			verified.ResolutionAuthority = creator.String()
			verified.OracleFeed = ""
			verified.OracleCondition = ""
			verified.OracleTargetPrice = 0
			verified.OracleTargetExpo = 0
		}
		return verified, nil
	}
	return VerifiedMarket{}, fmt.Errorf("create_market instruction not found in transaction")
}

func anchorDiscriminator(name string) [8]byte {
	hash := sha256.Sum256([]byte("global:" + name))
	var out [8]byte
	copy(out[:], hash[:8])
	return out
}

func decodeFixedString(buf []byte) string {
	return strings.TrimRight(strings.ReplaceAll(string(buf), "\x00", ""), " ")
}

func normalizeResolutionMode(raw byte) string {
	if raw == 1 {
		return "pyth"
	}
	return "creator"
}

func normalizeOracleCondition(raw byte) string {
	switch raw {
	case 0:
		return "gt"
	case 1:
		return "gte"
	case 2:
		return "lt"
	case 3:
		return "lte"
	default:
		return ""
	}
}
