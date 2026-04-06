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
	logger.Infof("market confirm received signature=%s", cmd.Signature)
	submission, err := s.repo.Load(context.Background(), cmd.Signature)
	if err != nil {
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
		logger.Warnf("market confirm mark watching failed signature=%s err=%v", submission.Signature, err)
		return true, false
	}
	ctx, cancel := chainconfirm.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	signature, err := solana.SignatureFromBase58(strings.TrimSpace(submission.Signature))
	if err != nil {
		if err := s.publishFailed(context.Background(), submission.Signature, "invalid_signature"); err != nil {
			logger.Warnf("publish market invalid-signature failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(context.Background(), submission.Signature, "invalid_signature"); err != nil {
			logger.Warnf("market mark failed invalid-signature failed signature=%s err=%v", submission.Signature, err)
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
		logger.Warnf("market confirm wait failed signature=%s err=%v", submission.Signature, err)
		if err := s.publishFailed(context.Background(), submission.Signature, reason); err != nil {
			logger.Warnf("publish market failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkExpired(context.Background(), submission.Signature, reason); err != nil {
			logger.Warnf("market mark expired failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	verified, err := VerifyMarketCreateTransaction(context.Background(), s.rpc, s.cfg, submission)
	if err != nil {
		logger.Warnf("market verify failed signature=%s err=%v", submission.Signature, err)
		if err := s.publishFailed(context.Background(), submission.Signature, "transaction_not_market_create"); err != nil {
			logger.Warnf("publish market verify-failed event failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		if err := s.repo.MarkFailed(context.Background(), submission.Signature, "transaction_not_market_create"); err != nil {
			logger.Warnf("market mark failed verify-failed failed signature=%s err=%v", submission.Signature, err)
			return true, false
		}
		return false, true
	}
	if result.Slot > 0 {
		verified.Slot = result.Slot
	}
	logger.Infof("market verified signature=%s market_id=%d market_pda=%s creator=%s metadata_cid=%s", verified.Signature, verified.MarketID, verified.MarketPDA, verified.Creator, verified.MetadataCID)
	if err := s.client.PublishJSON(context.Background(), protocol.SubjectMarketConfirmed, verified.Signature, verified.MarketConfirmedEvent); err != nil {
		logger.Warnf("publish market confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	if err := s.repo.MarkConfirmed(context.Background(), verified.MarketConfirmedEvent); err != nil {
		logger.Warnf("market mark confirmed failed signature=%s err=%v", verified.Signature, err)
		return true, false
	}
	logger.Infof("market confirm published signature=%s market_id=%d", verified.Signature, verified.MarketID)
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
