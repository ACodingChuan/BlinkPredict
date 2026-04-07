package marketconfirm

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

var logger = logging.New("marketconfirm")

const defaultConsumerName = "market-confirm"

const (
	marketConfirmFetchBatch = 16
	marketConfirmMaxWait    = 1500 * time.Millisecond
	marketRecoveryRetryBase = time.Second
	marketRecoveryRetryMax  = 20 * time.Second
)

type Service struct {
	client       *natsjs.Client
	repo         *Repository
	confirmer    *chainconfirm.Waiter
	rpc          *rpc.Client
	cfg          config.Config
	consumerName string
	sub          *nats.Subscription
	rootCtx      context.Context
	watchMu      sync.Mutex
	watchActive  map[string]struct{}
	retryBase    time.Duration
	retryLimit   time.Duration
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, cfg config.Config, routers ...chainconfirm.WSRouter) *Service {
	rpcClient := logging.NewSolanaRPCClient("marketconfirm-rpc", cfg.SolanaRPCURL)
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
		retryBase:    marketRecoveryRetryBase,
		retryLimit:   marketRecoveryRetryMax,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.repo == nil {
		return nil
	}
	s.rootCtx = ctx
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
	sub, err := s.client.PullSubscribe(protocol.SubjectMarketConfirm, s.consumerName)
	if err != nil {
		return fmt.Errorf("market confirm subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

func (s *Service) recoverActive(ctx context.Context) error {
	items, err := s.repo.ListActive(ctx)
	if err != nil {
		return fmt.Errorf("market confirm recover active: %w", err)
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
		msgs, err := s.sub.Fetch(marketConfirmFetchBatch, nats.MaxWait(marketConfirmMaxWait))
		if err != nil {
			if errors.Is(err, nats.ErrTimeout) || errors.Is(err, context.DeadlineExceeded) {
				continue
			}
			logger.Warnf("market confirm fetch failed: %v", err)
			time.Sleep(time.Second)
			continue
		}
		for _, msg := range msgs {
			s.handleMessage(msg)
		}
	}
}

func (s *Service) handleMessage(msg *nats.Msg) {
	var cmd protocol.MarketConfirmCommand
	if err := json.Unmarshal(msg.Data, &cmd); err != nil {
		logger.Warnf("market confirm decode failed: %v", err)
		_ = msg.Term()
		return
	}
	logger.Debugf("market confirm received signature=%s", cmd.Signature)
	submission, err := s.repo.Load(s.serviceContext(), cmd.Signature)
	if err != nil {
		if s.isStoppingError(err) {
			return
		}
		logger.Warnf("market confirm load failed signature=%s err=%v", cmd.Signature, err)
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
		if msg == nil {
			s.runRecoveryTask(signature, submission, s.processSubmission)
			return
		}
		retry, terminal := s.processSubmission(submission)
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

type submissionProcessor func(Submission) (retry bool, terminal bool)

func (s *Service) runRecoveryTask(signature string, submission Submission, process submissionProcessor) {
	if process == nil {
		return
	}
	delay := s.retryBaseDelay()
	for {
		retry, terminal := process(submission)
		if !retry || terminal {
			return
		}
		if !s.sleepRetry(delay) {
			return
		}
		if refreshed, ok := s.reloadSubmission(signature); ok {
			submission = refreshed
		}
		delay = nextRetryDelay(delay, s.retryLimitDelay())
	}
}

func (s *Service) reloadSubmission(signature string) (Submission, bool) {
	if s == nil || s.repo == nil {
		return Submission{}, false
	}
	updated, err := s.repo.Load(s.serviceContext(), signature)
	if err != nil {
		if s.isStoppingError(err) {
			return Submission{}, false
		}
		logger.Warnf("market confirm reload failed signature=%s err=%v", signature, err)
		return Submission{}, false
	}
	return updated, true
}

func (s *Service) retryBaseDelay() time.Duration {
	if s != nil && s.retryBase > 0 {
		return s.retryBase
	}
	return marketRecoveryRetryBase
}

func (s *Service) retryLimitDelay() time.Duration {
	if s != nil && s.retryLimit > 0 {
		return s.retryLimit
	}
	return marketRecoveryRetryMax
}

func (s *Service) sleepRetry(delay time.Duration) bool {
	ctx := context.Background()
	if s != nil && s.rootCtx != nil {
		ctx = s.rootCtx
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextRetryDelay(current time.Duration, max time.Duration) time.Duration {
	if current <= 0 {
		return max
	}
	if max <= 0 {
		return current
	}
	next := current * 2
	if next > max {
		return max
	}
	return next
}

func (s *Service) serviceContext() context.Context {
	if s != nil && s.rootCtx != nil {
		return s.rootCtx
	}
	return context.Background()
}

func (s *Service) isStoppingError(err error) bool {
	if err == nil || s == nil || s.rootCtx == nil || s.rootCtx.Err() == nil {
		return false
	}
	return errors.Is(err, context.Canceled)
}

func (s *Service) processSubmission(submission Submission) (retry bool, terminal bool) {
	if submission.Status == "confirmed" || submission.Status == "failed" || submission.Status == "expired" {
		return false, true
	}
	opCtx := s.serviceContext()
	if err := s.repo.MarkWatching(opCtx, submission.Signature); err != nil {
		if s.isStoppingError(err) {
			return true, false
		}
		logger.Warnf("market confirm mark watching failed signature=%s err=%v", submission.Signature, err)
		return true, false
	}
	ctx, cancel := chainconfirm.WithTimeout(opCtx, 90*time.Second)
	defer cancel()

	signature, err := solana.SignatureFromBase58(strings.TrimSpace(submission.Signature))
	if err != nil {
		if err := s.publishFailed(opCtx, submission.Signature, "invalid_signature"); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("publish market invalid-signature failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(opCtx, submission.Signature, "invalid_signature"); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("market mark failed invalid-signature failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, false
	}
	result, err := s.confirmer.WaitForConfirmed(ctx, signature)
	if err != nil {
		if s.isStoppingError(err) {
			return true, false
		}
		reason := "confirm_timeout"
		if !errors.Is(err, context.DeadlineExceeded) {
			reason = "confirm_failed"
		}
		logger.Warnf("market confirm wait failed signature=%s err=%v", submission.Signature, err)
		if err := s.publishFailed(opCtx, submission.Signature, reason); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("publish market failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkExpired(opCtx, submission.Signature, reason); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("market mark expired failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	verified, err := VerifyMarketCreateTransaction(opCtx, s.rpc, s.cfg, submission)
	if err != nil {
		if s.isStoppingError(err) || isRetryableVerifyError(err) {
			logger.Warnf("market verify deferred signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		logger.Warnf("market verify failed signature=%s err=%v", submission.Signature, err)
		if err := s.publishFailed(opCtx, submission.Signature, "transaction_not_market_create"); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("publish market verify-failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(opCtx, submission.Signature, "transaction_not_market_create"); err != nil {
			if s.isStoppingError(err) {
				return true, false
			}
			logger.Warnf("market mark failed verify-failed failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	if result.Slot > 0 {
		verified.Slot = result.Slot
	}
	logger.Debugf("market verified signature=%s market_id=%d market_pda=%s creator=%s metadata_cid=%s", verified.Signature, verified.MarketID, verified.MarketPDA, verified.Creator, verified.MetadataCID)
	if err := s.client.PublishJSON(opCtx, protocol.SubjectMarketConfirmed, verified.Signature, verified.MarketConfirmedEvent); err != nil {
		if s.isStoppingError(err) {
			return true, false
		}
		logger.Warnf("publish market confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	if err := s.repo.MarkConfirmed(opCtx, verified.MarketConfirmedEvent); err != nil {
		if s.isStoppingError(err) {
			return true, false
		}
		logger.Warnf("market mark confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	logger.Debugf("market confirm published signature=%s market_id=%d", verified.Signature, verified.MarketID)
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

func (s *Service) publishFailed(ctx context.Context, signature, reason string) error {
	if s == nil || s.client == nil {
		return nil
	}
	return s.client.PublishJSON(ctx, protocol.SubjectMarketFailed, signature+":"+reason, protocol.MarketFailedEvent{
		Signature: signature,
		Reason:    reason,
	})
}
