package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/protocol"
	internalsolana "blinkpredict/banckend/internal/solana"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var serviceLogger = logging.New("settlement")

const (
	defaultConsumerName = "settlement-primary"
	catchUpBatch        = 32
	runBatch            = 16
)

type Service struct {
	consumerName string
	client       *natsjs.Client
	rpc          *rpc.Client
	sub          *nats.Subscription
	registry     *UserPositionRegistry
	repo         *UserPositionAccountRepo
	checker      AccountExistenceChecker
	submitter    *Submitter
	programID    solana.PublicKey
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, rpcURL string, programID solana.PublicKey, relayer solana.PrivateKey, consumerName string) *Service {
	if consumerName == "" {
		consumerName = defaultConsumerName
	}
	rpcClient := logging.NewSolanaRPCClient("settlement-rpc", rpcURL)
	return &Service{
		consumerName: consumerName,
		client:       client,
		rpc:          rpcClient,
		registry:     NewUserPositionRegistry(),
		repo:         NewUserPositionAccountRepo(pool),
		checker:      &RPCAccountExistenceChecker{Client: rpcClient},
		submitter:    &Submitter{ProgramID: programID, Relayer: relayer, RPC: rpcClient},
		programID:    programID,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s.client == nil || s.submitter == nil {
		return nil
	}
	if err := LoadRegistryFromRepo(ctx, s.repo, s.registry); err != nil {
		return fmt.Errorf("load user position registry: %w", err)
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	if err := s.catchUp(ctx); err != nil {
		return err
	}
	go s.run(ctx)
	return nil
}

func (s *Service) ensureSubscription() error {
	if s.sub != nil {
		return nil
	}
	sub, err := s.client.PullSubscribe(protocol.SubjectMatchBatchV2+".*", s.consumerName)
	if err != nil {
		return fmt.Errorf("settlement subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

func (s *Service) catchUp(ctx context.Context) error {
	for {
		msgs, err := s.sub.Fetch(catchUpBatch, nats.MaxWait(500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		if len(msgs) == 0 {
			return nil
		}
		for _, msg := range msgs {
			s.handleMessage(ctx, msg)
		}
	}
}

func (s *Service) run(ctx context.Context) {
	defer func() {
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		msgs, err := s.sub.Fetch(runBatch, nats.MaxWait(1500*time.Millisecond))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			serviceLogger.Warnf("settlement fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			s.handleMessage(ctx, msg)
		}
	}
}

func (s *Service) handleMessage(ctx context.Context, msg *nats.Msg) {
	var event matching.MatchBatchEventV2
	if err := json.Unmarshal(msg.Data, &event); err != nil {
		serviceLogger.Warnf("decode settlement batch failed: %v", err)
		_ = msg.Term()
		return
	}
	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: s.programID})
	if err != nil {
		if errors.Is(err, ErrNoSettlementWork) {
			_ = msg.Ack()
			return
		}
		serviceLogger.Warnf("build submission batch failed market=%d err=%v", event.MarketID, err)
		_ = msg.Term()
		return
	}
	wallets := make([]string, 0, len(batch.UniqueUsers))
	for _, user := range batch.UniqueUsers {
		wallets = append(wallets, user.String())
	}
	plan, err := BuildUserPositionInitPlan(ctx, s.programID, batch.MarketID, batch.MarketPDA, wallets, s.registry, s.checker)
	if err != nil {
		serviceLogger.Warnf("build init plan failed market=%d err=%v", event.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	for _, entry := range plan.AlreadyExists {
		s.registry.MarkExists(entry.MarketID, entry.Wallet)
	}
	_, err = internalsolana.DeriveConfigPDA(s.programID)
	if err != nil {
		serviceLogger.Warnf("derive config pda failed err=%v", err)
		_ = msg.Term()
		return
	}
	tx, err := s.submitter.BuildTransaction(ctx, batch, plan)
	if err != nil {
		serviceLogger.Warnf("build settlement tx failed market=%d err=%v", event.MarketID, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	sig, err := s.submitter.Submit(ctx, tx)
	if err != nil {
		serviceLogger.Warnf("submit settlement tx failed market=%d err=%v", event.MarketID, err)
		_ = msg.NakWithDelay(2 * time.Second)
		return
	}
	if err := s.waitForConfirmation(ctx, sig, 45*time.Second); err != nil {
		serviceLogger.Warnf("settlement confirmation failed market=%d sig=%s err=%v", event.MarketID, sig.String(), err)
		_ = msg.NakWithDelay(2 * time.Second)
		return
	}
	if err := s.persistUserPositionObservations(ctx, plan, sig.String()); err != nil {
		serviceLogger.Warnf("persist user position observations failed market=%d sig=%s err=%v", event.MarketID, sig.String(), err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := s.publishSettlementConfirmed(ctx, batch, sig.String()); err != nil {
		serviceLogger.Warnf("publish settlement confirmation failed market=%d sig=%s err=%v", event.MarketID, sig.String(), err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	serviceLogger.Infof("confirmed settlement tx market=%d sig=%s fills=%d inits=%d", event.MarketID, sig.String(), len(batch.Fills), len(plan.NeedInit))
	_ = msg.Ack()
}

func (s *Service) waitForConfirmation(ctx context.Context, sig solana.Signature, timeout time.Duration) error {
	if s.rpc == nil {
		return fmt.Errorf("rpc client is not configured")
	}
	deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		result, err := s.rpc.GetSignatureStatuses(deadlineCtx, true, sig)
		if err == nil && result != nil && len(result.Value) > 0 && result.Value[0] != nil {
			status := result.Value[0]
			if status.Err != nil {
				return fmt.Errorf("transaction execution failed: %v", status.Err)
			}
			if status.ConfirmationStatus == rpc.ConfirmationStatusConfirmed || status.ConfirmationStatus == rpc.ConfirmationStatusFinalized {
				return nil
			}
		}

		select {
		case <-deadlineCtx.Done():
			return deadlineCtx.Err()
		case <-ticker.C:
		}
	}
}

func (s *Service) persistUserPositionObservations(ctx context.Context, plan UserPositionInitPlan, txSig string) error {
	if s.repo == nil {
		return nil
	}
	records := make([]UserPositionAccountRecord, 0, len(plan.AlreadyExists)+len(plan.NeedInit))
	for _, entry := range plan.AlreadyExists {
		s.registry.MarkExists(entry.MarketID, entry.Wallet)
		records = append(records, UserPositionAccountRecord{
			MarketID:        entry.MarketID,
			WalletAddress:   entry.Wallet,
			UserPositionPDA: entry.PositionPDA.String(),
		})
	}
	relayer := s.submitter.Relayer.PublicKey().String()
	for _, entry := range plan.NeedInit {
		s.registry.MarkExists(entry.MarketID, entry.Wallet)
		records = append(records, UserPositionAccountRecord{
			MarketID:         entry.MarketID,
			WalletAddress:    entry.Wallet,
			UserPositionPDA:  entry.PositionPDA.String(),
			CreatedByRelayer: relayer,
			CreatedTxSig:     txSig,
		})
	}
	return s.repo.UpsertObserved(ctx, records)
}

func (s *Service) publishSettlementConfirmed(ctx context.Context, batch SubmissionBatch, txSig string) error {
	if s.client == nil {
		return nil
	}
	wallets := make([]string, 0, len(batch.UniqueUsers))
	for _, user := range batch.UniqueUsers {
		wallets = append(wallets, user.String())
	}
	sort.Strings(wallets)
	event := protocol.SettlementConfirmedEvent{
		EventID:               fmt.Sprintf("settlement-confirmed:%d:%s", batch.MarketID, txSig),
		SchemaVersion:         1,
		MarketID:              batch.MarketID,
		MarketPDA:             batch.MarketPDA.String(),
		SettlementTxSignature: txSig,
		Wallets:               wallets,
		ConfirmedAt:           time.Now().UTC().Unix(),
	}
	return s.client.PublishJSON(ctx, protocol.SubjectSettlementConfirmedMarket(batch.MarketID), txSig, event)
}
