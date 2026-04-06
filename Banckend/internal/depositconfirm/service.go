package depositconfirm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/chainconfirm"
	"blinkpredict/banckend/internal/config"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/protocol"

	solana "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
)

var logger = logging.New("depositconfirm")

const defaultConsumerName = "deposit-confirm"

const (
	depositFetchBatch = 16
	depositMaxWait    = 1500 * time.Millisecond
)

type Service struct {
	client       *natsjs.Client
	repo         *Repository
	confirmer    *chainconfirm.Waiter
	rpc          *rpc.Client
	cfg          config.Config
	consumerName string
	sub          *nats.Subscription
	watchMu      sync.Mutex
	watchActive  map[string]struct{}
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, cfg config.Config, routers ...chainconfirm.WSRouter) *Service {
	rpcClient := logging.NewSolanaRPCClient("depositconfirm-rpc", cfg.SolanaRPCURL)
	var router chainconfirm.WSRouter
	if len(routers) > 0 {
		router = routers[0]
	}
	return &Service{
		client:       client,
		repo:         NewRepository(pool),
		confirmer:    chainconfirm.NewWaiter(rpcClient, cfg.SolanaWSURL, cfg.SolanaRPCURL, router),
		rpc:          rpcClient,
		cfg:          cfg,
		consumerName: defaultConsumerName,
		watchActive:  make(map[string]struct{}),
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.repo == nil {
		return nil
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	if err := s.recoverActive(ctx); err != nil {
		return err
	}
	go s.run(ctx)
	return nil
}

func (s *Service) ensureSubscription() error {
	if s.sub != nil {
		return nil
	}
	sub, err := s.client.PullSubscribe(protocol.SubjectDepositConfirm, s.consumerName)
	if err != nil {
		return fmt.Errorf("deposit confirm subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

func (s *Service) recoverActive(ctx context.Context) error {
	items, err := s.repo.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("deposit confirm recover active: %w", err)
	}
	for _, submission := range items {
		s.startSubmissionTask(submission, nil)
	}
	return nil
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
		msgs, err := s.sub.Fetch(depositFetchBatch, nats.MaxWait(depositMaxWait))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			logger.Warnf("deposit confirm fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			s.handleMessage(msg)
		}
	}
}

func (s *Service) handleMessage(msg *nats.Msg) {
	var cmd protocol.DepositConfirmCommand
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		logger.Warnf("deposit confirm decode failed: %v", err)
		_ = msg.Term()
		return
	}
	submission, err := s.repo.Load(context.Background(), cmd.Signature)
	if err != nil {
		logger.Warnf("deposit confirm load failed signature=%s err=%v", cmd.Signature, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if submission.Status == "confirmed" || submission.Status == "failed" || submission.Status == "expired" {
		_ = msg.Ack()
		return
	}
	s.startSubmissionTask(submission, msg)
}

func (s *Service) startSubmissionTask(submission Submission, msg *nats.Msg) {
	signature := strings.TrimSpace(submission.Signature)
	if signature == "" {
		if msg != nil {
			_ = msg.Term()
		}
		return
	}
	if !s.markActive(signature) {
		if msg != nil {
			_ = msg.NakWithDelay(time.Second)
		}
		return
	}
	go func() {
		defer s.clearActive(signature)
		retry, terminal := s.processSubmission(submission)
		if msg == nil {
			return
		}
		switch {
		case retry:
			_ = msg.NakWithDelay(time.Second)
		case terminal:
			_ = msg.Ack()
		default:
			_ = msg.Term()
		}
	}()
}

func (s *Service) processSubmission(submission Submission) (retry bool, terminal bool) {
	if submission.Status == "confirmed" || submission.Status == "failed" || submission.Status == "expired" {
		return false, true
	}
	if err := s.repo.MarkWatching(context.Background(), submission.Signature); err != nil {
		logger.Warnf("deposit confirm mark watching failed signature=%s err=%v", submission.Signature, err)
		return true, false
	}
	ctx, cancel := chainconfirm.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	signature, err := solana.SignatureFromBase58(strings.TrimSpace(submission.Signature))
	if err != nil {
		if err := s.publishFailed(context.Background(), submission.Signature, submission.WalletAddress, "invalid_signature"); err != nil {
			logger.Warnf("publish deposit invalid-signature failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(context.Background(), submission.Signature, "invalid_signature"); err != nil {
			logger.Warnf("deposit mark failed invalid-signature failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, false
	}
	result, err := s.confirmer.WaitForConfirmed(ctx, signature)
	if err != nil {
		reason := "confirm_timeout"
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			reason = "confirm_failed"
		}
		if err := s.publishFailed(context.Background(), submission.Signature, submission.WalletAddress, reason); err != nil {
			logger.Warnf("publish deposit failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkExpired(context.Background(), submission.Signature, reason); err != nil {
			logger.Warnf("deposit mark expired failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	verified, err := VerifyDepositTransaction(context.Background(), s.rpc, s.cfg, submission)
	if err != nil {
		logger.Warnf("deposit verify failed signature=%s err=%v", submission.Signature, err)
		if err := s.publishFailed(context.Background(), submission.Signature, submission.WalletAddress, "transaction_not_deposit"); err != nil {
			logger.Warnf("publish deposit verify-failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(context.Background(), submission.Signature, "transaction_not_deposit"); err != nil {
			logger.Warnf("deposit mark failed verify-failed failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	if result.Slot > 0 {
		verified.Slot = result.Slot
	}
	if err := s.client.PublishJSON(context.Background(), protocol.SubjectDepositConfirmed, verified.Signature, protocol.DepositConfirmedEvent{
		Signature:     verified.Signature,
		WalletAddress: verified.WalletAddress,
		AmountUnits:   verified.AmountUnits,
		Slot:          verified.Slot,
	}); err != nil {
		logger.Warnf("publish deposit confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	if err := s.repo.MarkConfirmed(context.Background(), verified.Signature, verified.Slot); err != nil {
		logger.Warnf("deposit mark confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	return false, true
}

func (s *Service) markActive(signature string) bool {
	s.watchMu.Lock()
	defer s.watchMu.Unlock()
	if _, exists := s.watchActive[signature]; exists {
		return false
	}
	s.watchActive[signature] = struct{}{}
	return true
}

func (s *Service) clearActive(signature string) {
	s.watchMu.Lock()
	delete(s.watchActive, signature)
	s.watchMu.Unlock()
}

func (s *Service) publishFailed(ctx context.Context, signature, wallet, reason string) error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.PublishJSON(ctx, protocol.SubjectDepositFailed, signature+":"+reason, protocol.DepositFailedEvent{
		Signature:     signature,
		WalletAddress: wallet,
		Reason:        reason,
	})
}
