package faucet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/programs/associated-token-account"
	"github.com/gagliardetto/solana-go/programs/token"
	"github.com/gagliardetto/solana-go/rpc"
	sendconfirm "github.com/gagliardetto/solana-go/rpc/sendAndConfirmTransaction"
	"github.com/gagliardetto/solana-go/rpc/ws"
)

type SolanaServiceConfig struct {
	RPCURL              string
	Mint                solana.PublicKey
	Decimals            int
	Payer               solana.PrivateKey
	MintAuthority       solana.PrivateKey
	AmountTokens        uint64
	Cooldown            time.Duration
	DisableRateLimit    bool
	AssociatedProgramID solana.PublicKey
}

type SolanaService struct {
	cfg  SolanaServiceConfig
	rpc  *rpc.Client
	ws   *ws.Client
	repo ClaimsRepository
}

func NewSolanaService(cfg SolanaServiceConfig, repo ClaimsRepository) (*SolanaService, error) {
	if cfg.RPCURL == "" {
		return nil, fmt.Errorf("rpc url is required")
	}
	if cfg.Mint.IsZero() {
		return nil, fmt.Errorf("mint is required")
	}
	if cfg.Decimals < 0 || cfg.Decimals > 18 {
		return nil, fmt.Errorf("invalid decimals: %d", cfg.Decimals)
	}
	if len(cfg.Payer) == 0 {
		return nil, fmt.Errorf("payer keypair is required")
	}
	if len(cfg.MintAuthority) == 0 {
		return nil, fmt.Errorf("mint authority keypair is required")
	}
	if cfg.AmountTokens == 0 {
		return nil, fmt.Errorf("amount must be > 0")
	}
	if cfg.Cooldown == 0 {
		cfg.Cooldown = 24 * time.Hour
	}
	if cfg.AssociatedProgramID.IsZero() {
		cfg.AssociatedProgramID = associatedtokenaccount.ProgramID
	}
	if repo == nil {
		return nil, fmt.Errorf("claims repo is required")
	}

	wsURL, err := rpcURLToWSURL(cfg.RPCURL)
	if err != nil {
		return nil, err
	}
	wsClient, err := ws.Connect(context.Background(), wsURL)
	if err != nil {
		return nil, fmt.Errorf("connect ws: %w", err)
	}
	return &SolanaService{
		cfg:  cfg,
		rpc:  rpc.New(cfg.RPCURL),
		ws:   wsClient,
		repo: repo,
	}, nil
}

func (s *SolanaService) Claim(ctx context.Context, solanaAddress string, ip string) (Result, error) {
	if strings.TrimSpace(solanaAddress) == "" || strings.TrimSpace(ip) == "" {
		return Result{}, fmt.Errorf("missing address or ip")
	}
	userWallet, err := solana.PublicKeyFromBase58(solanaAddress)
	if err != nil {
		return Result{}, fmt.Errorf("invalid solana address: %w", err)
	}

	now := time.Now().UTC()
	if !s.cfg.DisableRateLimit {
		if at, ok, err := s.repo.LastClaimedAtByWallet(ctx, solanaAddress); err != nil {
			return Result{}, err
		} else if ok && now.Sub(at) < s.cfg.Cooldown {
			return Result{}, WrapRateLimit(nil, at.Add(s.cfg.Cooldown))
		}
		if at, ok, err := s.repo.LastClaimedAtByIP(ctx, ip); err != nil {
			return Result{}, err
		} else if ok && now.Sub(at) < s.cfg.Cooldown {
			return Result{}, WrapRateLimit(nil, at.Add(s.cfg.Cooldown))
		}
	}

	tokenProgramID, err := s.detectMintTokenProgram(ctx)
	if err != nil {
		return Result{}, err
	}

	// Derive the user's vUSDC ATA (NOT the treasury/payer's ATA).
	userATA, err := findAssociatedTokenAddress(userWallet, s.cfg.Mint, tokenProgramID)
	if err != nil {
		return Result{}, fmt.Errorf("derive user ata: %w", err)
	}

	// Check if the user's ATA already exists; create it if missing.
	ataExists := true
	if _, err := s.rpc.GetAccountInfo(ctx, userATA); err != nil {
		if errors.Is(err, rpc.ErrNotFound) {
			ataExists = false
		} else {
			return Result{}, fmt.Errorf("get ata account info: %w", err)
		}
	}

	instructions := make([]solana.Instruction, 0, 2)
	if !ataExists {
		// Payer (relayer) funds the ATA rent; owner is the user.
		instructions = append(instructions, buildCreateATAInstruction(
			s.cfg.Payer.PublicKey(), // payer (rent funder)
			userWallet,              // ATA owner = the user
			s.cfg.Mint,
			userATA,
			tokenProgramID,
			s.cfg.AssociatedProgramID,
		))
	}

	amountBaseUnits, err := s.amountBaseUnits()
	if err != nil {
		return Result{}, err
	}

	// Mint vUSDC directly to the user's ATA.
	mintTo := token.NewMintToInstruction(
		amountBaseUnits,
		s.cfg.Mint,
		userATA, // destination = user's ATA
		s.cfg.MintAuthority.PublicKey(),
		nil,
	).Build()
	mintToData, err := mintTo.Data()
	if err != nil {
		return Result{}, fmt.Errorf("encode mint_to instruction: %w", err)
	}
	instructions = append(instructions, solana.NewInstruction(
		tokenProgramID,
		mintTo.Accounts(),
		mintToData,
	))

	latest, err := s.rpc.GetLatestBlockhash(ctx, rpc.CommitmentFinalized)
	if err != nil {
		return Result{}, fmt.Errorf("get latest blockhash: %w", err)
	}

	tx, err := solana.NewTransaction(
		instructions,
		latest.Value.Blockhash,
		solana.TransactionPayer(s.cfg.Payer.PublicKey()),
	)
	if err != nil {
		return Result{}, fmt.Errorf("build transaction: %w", err)
	}

	// Sign with payer + mint authority. If they are the same keypair, that's fine.
	_, err = tx.Sign(func(key solana.PublicKey) *solana.PrivateKey {
		if key.Equals(s.cfg.Payer.PublicKey()) {
			return &s.cfg.Payer
		}
		if key.Equals(s.cfg.MintAuthority.PublicKey()) {
			return &s.cfg.MintAuthority
		}
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("sign transaction: %w", err)
	}

	sig, err := sendconfirm.SendAndConfirmTransactionWithOpts(ctx, s.rpc, s.ws, tx, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentProcessed,
	}, nil)
	if err != nil {
		return Result{}, fmt.Errorf("send transaction: %w", err)
	}

	if err := s.repo.InsertClaim(ctx, ClaimRow{
		SolanaAddress: solanaAddress,
		IP:            ip,
		Signature:     sig.String(),
		Amount:        s.cfg.AmountTokens,
		Mint:          s.cfg.Mint.String(),
		ATA:           userATA.String(),
		ClaimedAt:     now,
	}); err != nil {
		// If DB write fails, we still return the signature so the user can verify/move on.
		return Result{Signature: sig.String(), Mint: s.cfg.Mint.String(), ATA: userATA.String(), Amount: s.cfg.AmountTokens, ClaimedAt: now},
			fmt.Errorf("record claim: %w", err)
	}

	return Result{
		Signature: sig.String(),
		Mint:      s.cfg.Mint.String(),
		ATA:       userATA.String(),
		Amount:    s.cfg.AmountTokens,
		ClaimedAt: now,
	}, nil
}

func rpcURLToWSURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	switch parsed.Scheme {
	case "https":
		parsed.Scheme = "wss"
	default:
		parsed.Scheme = "ws"
	}
	return parsed.String(), nil
}

func divisorForLedgerUnits(decimals int) uint64 {
	if decimals <= 2 {
		return 1
	}
	divisor := uint64(1)
	for i := 0; i < decimals-2; i++ {
		divisor *= 10
	}
	return divisor
}

func (s *SolanaService) amountBaseUnits() (uint64, error) {
	if s.cfg.Decimals < 0 || s.cfg.Decimals > 18 {
		return 0, fmt.Errorf("invalid decimals: %d", s.cfg.Decimals)
	}
	multiplier := uint64(1)
	for i := 0; i < s.cfg.Decimals; i += 1 {
		if multiplier > (^uint64(0))/10 {
			return 0, fmt.Errorf("decimals multiplier overflow")
		}
		multiplier *= 10
	}
	if s.cfg.AmountTokens > (^uint64(0))/multiplier {
		return 0, fmt.Errorf("amount too large")
	}
	return s.cfg.AmountTokens * multiplier, nil
}

func (s *SolanaService) detectMintTokenProgram(ctx context.Context) (solana.PublicKey, error) {
	info, err := s.rpc.GetAccountInfo(ctx, s.cfg.Mint)
	if err != nil {
		return solana.PublicKey{}, fmt.Errorf("get mint account info: %w", err)
	}
	if info == nil || info.Value == nil {
		return solana.PublicKey{}, fmt.Errorf("mint account not found")
	}
	owner := info.Value.Owner
	if owner.Equals(solana.TokenProgramID) || owner.Equals(solana.Token2022ProgramID) {
		return owner, nil
	}
	return solana.PublicKey{}, fmt.Errorf("unsupported token program for mint: %s", owner.String())
}

func findAssociatedTokenAddress(wallet, mint, tokenProgramID solana.PublicKey) (solana.PublicKey, error) {
	ata, _, err := solana.FindProgramAddress(
		[][]byte{
			wallet[:],
			tokenProgramID[:],
			mint[:],
		},
		solana.SPLAssociatedTokenAccountProgramID,
	)
	if err != nil {
		return solana.PublicKey{}, err
	}
	return ata, nil
}

func buildCreateATAInstruction(
	payer solana.PublicKey,
	wallet solana.PublicKey,
	mint solana.PublicKey,
	ata solana.PublicKey,
	tokenProgramID solana.PublicKey,
	associatedProgramID solana.PublicKey,
) solana.Instruction {
	accounts := solana.AccountMetaSlice{
		{PublicKey: payer, IsSigner: true, IsWritable: true},
		{PublicKey: ata, IsSigner: false, IsWritable: true},
		{PublicKey: wallet, IsSigner: false, IsWritable: false},
		{PublicKey: mint, IsSigner: false, IsWritable: false},
		{PublicKey: solana.SystemProgramID, IsSigner: false, IsWritable: false},
		{PublicKey: tokenProgramID, IsSigner: false, IsWritable: false},
	}
	// Associated token create instruction has empty data payload.
	return solana.NewInstruction(associatedProgramID, accounts, []byte{})
}

// LoadKeypair parses either a solana-keygen JSON array or a file path to that JSON.
func LoadKeypair(value string) (solana.PrivateKey, error) {
	raw := strings.TrimSpace(value)
	if raw == "" {
		return solana.PrivateKey{}, fmt.Errorf("empty keypair")
	}
	if strings.HasPrefix(raw, "[") {
		return parseKeypairJSON([]byte(raw))
	}
	// Treat as file path.
	content, err := os.ReadFile(raw)
	if err != nil {
		return solana.PrivateKey{}, fmt.Errorf("read keypair file: %w", err)
	}
	return parseKeypairJSON(content)
}

func parseKeypairJSON(content []byte) (solana.PrivateKey, error) {
	var bytesArr []byte
	var ints []int
	if err := json.Unmarshal(content, &bytesArr); err == nil && len(bytesArr) == 64 {
		return solana.PrivateKey(bytesArr), nil
	}
	if err := json.Unmarshal(content, &ints); err != nil {
		return solana.PrivateKey{}, fmt.Errorf("invalid keypair json")
	}
	if len(ints) != 64 {
		return solana.PrivateKey{}, fmt.Errorf("invalid keypair length: %d", len(ints))
	}
	buf := make([]byte, 64)
	for i, v := range ints {
		if v < 0 || v > 255 {
			return solana.PrivateKey{}, fmt.Errorf("invalid keypair byte at %d", i)
		}
		buf[i] = byte(v)
	}
	return solana.PrivateKey(buf), nil
}
