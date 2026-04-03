package marketconfirm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
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

const defaultConsumerName = "market-confirm-primary"

type Service struct {
	client       *natsjs.Client
	repo         *Repository
	confirmer    *chainconfirm.Waiter
	rpc          *rpc.Client
	cfg          config.Config
	consumerName string
	sub          *nats.Subscription
}

func NewService(client *natsjs.Client, pool *pgxpool.Pool, cfg config.Config) *Service {
	rpcClient := logging.NewSolanaRPCClient("marketconfirm-rpc", cfg.SolanaRPCURL)
	return &Service{
		client:       client,
		repo:         NewRepository(pool),
		confirmer:    chainconfirm.NewWaiter(rpcClient, cfg.SolanaWSURL, cfg.SolanaRPCURL),
		rpc:          rpcClient,
		cfg:          cfg,
		consumerName: defaultConsumerName,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.repo == nil {
		return nil
	}
	if s.sub != nil {
		return nil
	}
	sub, err := s.client.JetStream().QueueSubscribe(
		protocol.SubjectMarketConfirm,
		"market_confirm_group",
		s.handleMessage,
		nats.Durable(s.consumerName),
		nats.ManualAck(),
		nats.DeliverAll(),
	)
	if err != nil {
		return fmt.Errorf("market confirm subscribe: %w", err)
	}
	s.sub = sub
	go func() {
		<-ctx.Done()
		if s.sub != nil {
			_ = s.sub.Unsubscribe()
		}
	}()
	return nil
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
	if submission.Status == "confirmed" {
		_ = msg.Ack()
		return
	}
	if err := s.repo.MarkWatching(context.Background(), cmd.Signature); err != nil {
		logger.Warnf("market confirm mark watching failed signature=%s err=%v", cmd.Signature, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	ctx, cancel := chainconfirm.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	signature, err := solana.SignatureFromBase58(strings.TrimSpace(cmd.Signature))
	if err != nil {
		_ = s.repo.MarkFailed(context.Background(), cmd.Signature, "invalid_signature")
		_ = s.publishFailed(context.Background(), cmd.Signature, "invalid_signature")
		_ = msg.Term()
		return
	}
	result, err := s.confirmer.WaitForConfirmed(ctx, signature)
	if err != nil {
		reason := "confirm_timeout"
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
			reason = "confirm_failed"
		}
		logger.Warnf("market confirm wait failed signature=%s err=%v", cmd.Signature, err)
		_ = s.repo.MarkExpired(context.Background(), cmd.Signature, reason)
		_ = s.publishFailed(context.Background(), cmd.Signature, reason)
		_ = msg.Ack()
		return
	}
	verified, err := VerifyMarketCreateTransaction(context.Background(), s.rpc, s.cfg, submission)
	if err != nil {
		logger.Warnf("market verify failed signature=%s err=%v", cmd.Signature, err)
		_ = s.repo.MarkFailed(context.Background(), cmd.Signature, "transaction_not_market_create")
		_ = s.publishFailed(context.Background(), cmd.Signature, "transaction_not_market_create")
		_ = msg.Ack()
		return
	}
	if result.Slot > 0 {
		verified.Slot = result.Slot
	}
	logger.Infof("market verified signature=%s market_id=%d market_pda=%s creator=%s metadata_cid=%s", verified.Signature, verified.MarketID, verified.MarketPDA, verified.Creator, verified.MetadataCID)
	if err := s.repo.MarkConfirmed(context.Background(), verified.MarketConfirmedEvent); err != nil {
		logger.Warnf("market mark confirmed failed signature=%s err=%v", verified.Signature, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	if err := s.client.PublishJSON(context.Background(), protocol.SubjectMarketConfirmed, verified.Signature, verified.MarketConfirmedEvent); err != nil {
		logger.Warnf("publish market confirmed failed signature=%s err=%v", verified.Signature, err)
		_ = msg.NakWithDelay(time.Second)
		return
	}
	logger.Infof("market confirm published signature=%s market_id=%d", verified.Signature, verified.MarketID)
	_ = msg.Ack()
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
