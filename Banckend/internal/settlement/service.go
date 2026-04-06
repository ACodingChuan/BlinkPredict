package settlement

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"blinkpredict/banckend/internal/bus/natsjs"
	"blinkpredict/banckend/internal/chainconfirm"
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
	defaultConsumerName      = "settlement-execution"
	catchUpBatch             = 32
	runBatch                 = 16
	submittedStatusPoll      = 2 * time.Second
	submittedRebroadcast     = 5 * time.Second
	submittedBlockPoll       = 2 * time.Second
	defaultReconcileInterval = 10 * time.Second
	defaultSubmitStepTimeout = 15 * time.Second
	schedulerFallbackScan    = 2 * time.Second
	settlementSchemaNumber   = 1
)

type marketLane struct {
	MarketID            uint64
	Paused              bool
	CurrentMatchEventID string
}

type Service struct {
	consumerName string
	client       *natsjs.Client
	pool         *pgxpool.Pool
	rpc          *rpc.Client
	router       chainconfirm.WSRouter
	sub          *nats.Subscription

	registry       *UserPositionRegistry
	accountRepo    *UserPositionAccountRepo
	checker        AccountExistenceChecker
	submitter      *Submitter
	submissionRepo *submissionRepo
	programID      solana.PublicKey

	lanesMu      sync.Mutex
	lanes        map[uint64]*marketLane
	dirtyMu      sync.Mutex
	dirtyMarkets map[uint64]struct{}
	dirtyWake    chan struct{}

	watchMu      sync.Mutex
	watchCancels map[string]context.CancelFunc

	reconcileInterval time.Duration
}

func NewService(
	client *natsjs.Client,
	pool *pgxpool.Pool,
	rpcURL string,
	programID solana.PublicKey,
	relayer solana.PrivateKey,
	router chainconfirm.WSRouter,
	consumerName string,
	reconcileInterval time.Duration,
) *Service {
	if consumerName == "" {
		consumerName = defaultConsumerName
	}
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}
	rpcClient := logging.NewSolanaRPCClient("settlement-rpc", rpcURL)
	return &Service{
		consumerName:      consumerName,
		client:            client,
		pool:              pool,
		rpc:               rpcClient,
		router:            router,
		registry:          NewUserPositionRegistry(),
		accountRepo:       NewUserPositionAccountRepo(pool),
		checker:           &RPCAccountExistenceChecker{Client: rpcClient},
		submitter:         &Submitter{ProgramID: programID, Relayer: relayer, RPC: rpcClient},
		submissionRepo:    newSubmissionRepo(pool),
		programID:         programID,
		lanes:             make(map[uint64]*marketLane),
		dirtyMarkets:      make(map[uint64]struct{}),
		dirtyWake:         make(chan struct{}, 1),
		watchCancels:      make(map[string]context.CancelFunc),
		reconcileInterval: reconcileInterval,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.submitter == nil || s.submissionRepo == nil {
		return nil
	}
	if err := LoadRegistryFromRepo(ctx, s.accountRepo, s.registry); err != nil {
		return fmt.Errorf("load user position registry: %w", err)
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	if err := s.recoverState(ctx); err != nil {
		return err
	}
	go s.runIngress(ctx)
	go s.runScheduler(ctx)
	go s.runReconciler(ctx)
	return nil
}

func (s *Service) ensureSubscription() error {
	if s.sub != nil {
		return nil
	}
	sub, err := s.client.PullSubscribe(protocol.SubjectMatchExecution+".*", s.consumerName)
	if err != nil {
		return fmt.Errorf("settlement subscribe: %w", err)
	}
	s.sub = sub
	return nil
}

func (s *Service) recoverState(ctx context.Context) error {
	quietCtx := logging.WithoutPGXQueryLogging(ctx)
	paused, err := s.submissionRepo.ListPausedMarketIDs(ctx)
	if err != nil {
		return fmt.Errorf("settlement load paused markets: %w", err)
	}
	for _, marketID := range paused {
		s.ensureLane(marketID).Paused = true
	}
	submitted, err := s.submissionRepo.ListSubmitted(quietCtx)
	if err != nil {
		return fmt.Errorf("settlement load submitted rows: %w", err)
	}
	for _, record := range submitted {
		lane := s.ensureLane(record.MarketID)
		lane.CurrentMatchEventID = record.MatchEventID
		s.startWatchTask(ctx, record)
	}
	queuedMarkets, err := s.submissionRepo.ListQueuedMarketIDs(quietCtx)
	if err != nil {
		return fmt.Errorf("settlement load queued markets: %w", err)
	}
	for _, marketID := range queuedMarkets {
		s.markDirty(marketID)
	}
	_, err = s.republishUnpublishedEvents(quietCtx)
	return err
}

func (s *Service) runIngress(ctx context.Context) {
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
		s.handleFetchedMessages(ctx, msgs)
	}
}

func (s *Service) handleFetchedMessages(ctx context.Context, msgs []*nats.Msg) {
	type validMsg struct {
		msg       *nats.Msg
		event     matching.MatchBatchEvent
		insertRow queuedInsert
	}
	valid := make([]validMsg, 0, len(msgs))
	for _, msg := range msgs {
		var event matching.MatchBatchEvent
		if err := json.Unmarshal(msg.Data, &event); err != nil {
			serviceLogger.Warnf("decode settlement batch failed: %v", err)
			_ = msg.Term()
			continue
		}
		if len(event.Fills) == 0 {
			_ = msg.Ack()
			continue
		}
		matchEventID := strings.TrimSpace(event.EventID)
		if matchEventID == "" {
			matchEventID = fmt.Sprintf("match-batch-%d-%d-%d", event.MarketID, event.SourceCmdSeqMax, event.ProducedAt)
			event.EventID = matchEventID
		}
		wallets := walletsForSubmissionEvent(event)
		eventJSON, err := json.Marshal(event)
		if err != nil {
			serviceLogger.Warnf("marshal settlement match batch failed event=%s err=%v", matchEventID, err)
			_ = msg.Term()
			continue
		}
		walletsJSON, err := json.Marshal(wallets)
		if err != nil {
			serviceLogger.Warnf("marshal settlement wallets failed event=%s err=%v", matchEventID, err)
			_ = msg.Term()
			continue
		}
		laneStatus := string(LaneActive)
		if s.isLanePaused(event.MarketID) {
			laneStatus = string(LanePaused)
		}
		valid = append(valid, validMsg{
			msg:   msg,
			event: event,
			insertRow: queuedInsert{
				MatchEventID:     matchEventID,
				MarketID:         event.MarketID,
				MarketPDA:        strings.TrimSpace(event.MarketPDA),
				MatchEventJSON:   eventJSON,
				WalletsJSON:      walletsJSON,
				MarketLaneStatus: laneStatus,
			},
		})
	}
	if len(valid) == 0 {
		return
	}
	rows := make([]queuedInsert, 0, len(valid))
	for _, item := range valid {
		rows = append(rows, item.insertRow)
	}
	inserted, err := s.submissionRepo.UpsertQueuedBatch(ctx, rows)
	if err != nil {
		serviceLogger.Warnf("queue settlement submissions failed: %v", err)
		for _, item := range valid {
			_ = item.msg.NakWithDelay(time.Second)
		}
		return
	}
	for i, item := range valid {
		if inserted[i] {
			s.markDirty(item.event.MarketID)
		}
		_ = item.msg.Ack()
	}
}

func (s *Service) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(schedulerFallbackScan)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.dirtyWake:
			s.processDirtyMarkets(ctx)
		case <-ticker.C:
			s.processDirtyMarkets(ctx)
		}
	}
}

func (s *Service) processDirtyMarkets(ctx context.Context) {
	markets := s.drainDirtyMarkets()
	for _, marketID := range markets {
		s.tryScheduleMarket(ctx, marketID)
	}
}

func (s *Service) tryScheduleMarket(ctx context.Context, marketID uint64) {
	lane := s.ensureLane(marketID)
	s.lanesMu.Lock()
	if lane.Paused || lane.CurrentMatchEventID != "" {
		s.lanesMu.Unlock()
		return
	}
	s.lanesMu.Unlock()

	record, ok, err := s.submissionRepo.LoadNextQueuedByMarket(ctx, marketID)
	if err != nil {
		serviceLogger.Warnf("load queued settlement row failed market=%d err=%v", marketID, err)
		return
	}
	if !ok {
		return
	}

	s.lanesMu.Lock()
	lane = s.ensureLaneLocked(record.MarketID)
	if lane.Paused || lane.CurrentMatchEventID != "" {
		s.lanesMu.Unlock()
		return
	}
	lane.CurrentMatchEventID = record.MatchEventID
	s.lanesMu.Unlock()

	go s.submitMatch(ctx, record.MatchEventID)
}

func (s *Service) submitMatch(ctx context.Context, matchEventID string) {
	record, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, matchEventID)
	if err != nil || !ok {
		serviceLogger.Warnf("load settlement record failed match_event_id=%s err=%v", matchEventID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		return
	}
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(record.MatchEventJSON, &event); err != nil {
		serviceLogger.Warnf("decode queued settlement event failed match_event_id=%s err=%v", matchEventID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		return
	}
	serviceLogger.Infof("settlement submit started match_event_id=%s market=%d wallets=%d fills=%d", matchEventID, event.MarketID, len(record.Wallets), len(event.Fills))
	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: s.programID})
	if err != nil {
		serviceLogger.Warnf("build submission batch failed match_event_id=%s market=%d err=%v", matchEventID, event.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		return
	}
	planCtx, cancel := chainconfirm.WithTimeout(ctx, defaultSubmitStepTimeout)
	plan, err := BuildUserPositionInitPlan(planCtx, s.programID, batch.MarketID, batch.MarketPDA, record.Wallets, s.registry, s.checker)
	cancel()
	if err != nil {
		serviceLogger.Warnf("build init plan failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	serviceLogger.Infof("settlement init plan built match_event_id=%s market=%d known=%d unknown=%d init=%d exists=%d", matchEventID, batch.MarketID, len(plan.KnownWallets), len(plan.UnknownWallets), len(plan.NeedInit), len(plan.AlreadyExists))
	signCtx, cancel := chainconfirm.WithTimeout(ctx, defaultSubmitStepTimeout)
	tx, sig, rawTx, lastValidBlockHeight, err := s.submitter.BuildSignedTransaction(signCtx, batch, plan)
	cancel()
	if err != nil {
		serviceLogger.Warnf("build signed settlement tx failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	serviceLogger.Infof("settlement tx signed match_event_id=%s market=%d sig=%s last_valid_block_height=%d", matchEventID, batch.MarketID, sig.String(), lastValidBlockHeight)
	applied, err := s.submissionRepo.MarkSubmittedCAS(ctx, matchEventID, sig.String(), rawTx, lastValidBlockHeight)
	if err != nil {
		serviceLogger.Warnf("mark settlement submitted failed match_event_id=%s err=%v", matchEventID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	if !applied {
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	serviceLogger.Infof("settlement row marked submitted match_event_id=%s market=%d sig=%s", matchEventID, batch.MarketID, sig.String())
	record, _, _ = s.submissionRepo.LoadByMatchEventID(ctx, matchEventID)
	if err := s.publishSubmitted(ctx, record); err != nil {
		serviceLogger.Warnf("publish settlement submitted failed match_event_id=%s err=%v", matchEventID, err)
	} else {
		_ = s.submissionRepo.MarkSubmittedEventPublished(ctx, matchEventID)
	}
	broadcastCtx, cancel := chainconfirm.WithTimeout(ctx, defaultSubmitStepTimeout)
	err = s.broadcastRawTx(broadcastCtx, rawTx)
	cancel()
	if err != nil {
		serviceLogger.Warnf("broadcast settlement raw tx failed match_event_id=%s market=%d sig=%s err=%v", matchEventID, batch.MarketID, sig.String(), err)
		if isDeterministicSettlementError(err) {
			s.failSubmission(ctx, matchEventID, batch.MarketID, sig.String(), "simulation_failed")
			return
		}
	} else {
		serviceLogger.Infof("settlement raw tx broadcast match_event_id=%s market=%d sig=%s", matchEventID, batch.MarketID, sig.String())
	}
	_ = tx
	s.startWatchTask(ctx, record)
}

func (s *Service) startWatchTask(parent context.Context, record SubmissionRecord) {
	if record.MatchEventID == "" || record.Status != string(StatusSubmitted) {
		return
	}
	s.watchMu.Lock()
	if _, exists := s.watchCancels[record.MatchEventID]; exists {
		s.watchMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	s.watchCancels[record.MatchEventID] = cancel
	s.watchMu.Unlock()

	go func(initial SubmissionRecord) {
		defer func() {
			s.watchMu.Lock()
			delete(s.watchCancels, initial.MatchEventID)
			s.watchMu.Unlock()
		}()
		s.watchSubmission(ctx, initial)
	}(record)
}

func (s *Service) watchSubmission(ctx context.Context, record SubmissionRecord) {
	current := record
	for {
		status, terminal, err := s.checkSignatureStatus(ctx, current.TxSignature)
		if err == nil && terminal {
			if status.ErrText != "" {
				s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed")
			} else {
				s.confirmSubmission(ctx, current, status.Slot)
			}
			return
		}

		var (
			routerCh    chan chainconfirm.SignatureResult
			unsubscribe func()
		)
		if s.router != nil {
			routerCh = make(chan chainconfirm.SignatureResult, 1)
			unsub, subErr := s.router.SubscribeSignature(current.TxSignature, "settlement:"+current.MatchEventID, "settlement", routerCh)
			if subErr == nil {
				unsubscribe = unsub
			} else {
				serviceLogger.Warnf("settlement router subscribe failed match_event_id=%s sig=%s err=%v", current.MatchEventID, current.TxSignature, subErr)
			}
		}
		statusTicker := time.NewTicker(submittedStatusPoll)
		rebroadcastTicker := time.NewTicker(submittedRebroadcast)
		heightTicker := time.NewTicker(submittedBlockPoll)
		for {
			select {
			case <-ctx.Done():
				if unsubscribe != nil {
					unsubscribe()
				}
				statusTicker.Stop()
				rebroadcastTicker.Stop()
				heightTicker.Stop()
				return
			case res := <-routerCh:
				if unsubscribe != nil {
					unsubscribe()
				}
				statusTicker.Stop()
				rebroadcastTicker.Stop()
				heightTicker.Stop()
				if res.ErrText != "" {
					s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed")
				} else {
					s.confirmSubmission(ctx, current, res.Slot)
				}
				return
			case <-statusTicker.C:
				status, terminal, err := s.checkSignatureStatus(ctx, current.TxSignature)
				if err != nil || !terminal {
					continue
				}
				if unsubscribe != nil {
					unsubscribe()
				}
				statusTicker.Stop()
				rebroadcastTicker.Stop()
				heightTicker.Stop()
				if status.ErrText != "" {
					s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed")
				} else {
					s.confirmSubmission(ctx, current, status.Slot)
				}
				return
			case <-rebroadcastTicker.C:
				if err := s.broadcastRawTx(ctx, current.RawTxBase64); err != nil {
					serviceLogger.Warnf("rebroadcast settlement raw tx failed match_event_id=%s sig=%s err=%v", current.MatchEventID, current.TxSignature, err)
				}
			case <-heightTicker.C:
				expired, err := s.isExpired(ctx, current.LastValidBlockHeight)
				if err != nil || !expired {
					continue
				}
				next, replaced, resignErr := s.resignSubmission(ctx, current)
				if resignErr != nil {
					serviceLogger.Warnf("resign settlement tx failed match_event_id=%s sig=%s err=%v", current.MatchEventID, current.TxSignature, resignErr)
					continue
				}
				if !replaced {
					latest, ok, loadErr := s.submissionRepo.LoadByMatchEventID(ctx, current.MatchEventID)
					if loadErr == nil && ok && latest.Status == string(StatusSubmitted) {
						current = latest
						if unsubscribe != nil {
							unsubscribe()
						}
						statusTicker.Stop()
						rebroadcastTicker.Stop()
						heightTicker.Stop()
						goto RESUBSCRIBE
					}
					continue
				}
				current = next
				if unsubscribe != nil {
					unsubscribe()
				}
				statusTicker.Stop()
				rebroadcastTicker.Stop()
				heightTicker.Stop()
				goto RESUBSCRIBE
			}
		}
	RESUBSCRIBE:
		continue
	}
}

func (s *Service) resignSubmission(ctx context.Context, current SubmissionRecord) (SubmissionRecord, bool, error) {
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(current.MatchEventJSON, &event); err != nil {
		return SubmissionRecord{}, false, err
	}
	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: s.programID})
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	plan, err := BuildUserPositionInitPlan(ctx, s.programID, batch.MarketID, batch.MarketPDA, current.Wallets, s.registry, s.checker)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	_, sig, rawTx, lastValidBlockHeight, err := s.submitter.BuildSignedTransaction(ctx, batch, plan)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	replaced, err := s.submissionRepo.ReplaceSignatureCAS(ctx, current.MatchEventID, current.TxSignature, sig.String(), rawTx, lastValidBlockHeight)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	if !replaced {
		return SubmissionRecord{}, false, nil
	}
	latest, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, current.MatchEventID)
	if err != nil || !ok {
		return SubmissionRecord{}, false, err
	}
	if err := s.publishSubmitted(ctx, latest); err != nil {
		serviceLogger.Warnf("publish settlement resubmitted failed match_event_id=%s err=%v", current.MatchEventID, err)
	}
	if err := s.broadcastRawTx(ctx, rawTx); err != nil {
		serviceLogger.Warnf("broadcast resigned settlement raw tx failed match_event_id=%s sig=%s err=%v", current.MatchEventID, sig.String(), err)
		if isDeterministicSettlementError(err) {
			s.failSubmission(ctx, current.MatchEventID, current.MarketID, sig.String(), "simulation_failed")
		}
	}
	return latest, true, nil
}

func (s *Service) confirmSubmission(ctx context.Context, record SubmissionRecord, slot uint64) {
	applied, err := s.submissionRepo.MarkConfirmedCAS(ctx, record.MatchEventID, record.TxSignature, slot)
	if err != nil {
		serviceLogger.Warnf("mark settlement confirmed failed match_event_id=%s err=%v", record.MatchEventID, err)
		return
	}
	if !applied {
		return
	}
	if err := s.persistObservedUserPositions(ctx, record); err != nil {
		serviceLogger.Warnf("persist settlement user positions failed match_event_id=%s err=%v", record.MatchEventID, err)
	}
	latest, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, record.MatchEventID)
	if err == nil && ok {
		record = latest
	}
	if err := s.publishConfirmed(ctx, record, slot); err != nil {
		serviceLogger.Warnf("publish settlement confirmed failed match_event_id=%s err=%v", record.MatchEventID, err)
	} else {
		_ = s.submissionRepo.MarkTerminalEventPublished(ctx, record.MatchEventID)
	}
	s.releaseLane(record.MarketID, false, true)
}

func (s *Service) failSubmission(ctx context.Context, matchEventID string, marketID uint64, txSignature string, reasonCode string) {
	applied, failedMarketID, err := s.submissionRepo.MarkFailedAndPauseQueued(ctx, matchEventID, txSignature, reasonCode)
	if err != nil {
		serviceLogger.Warnf("mark settlement failed failed match_event_id=%s err=%v", matchEventID, err)
		return
	}
	if !applied {
		return
	}
	record, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, matchEventID)
	if err == nil && ok {
		if err := s.publishFailed(ctx, record); err != nil {
			serviceLogger.Warnf("publish settlement failed event failed match_event_id=%s err=%v", matchEventID, err)
		} else {
			_ = s.submissionRepo.MarkTerminalEventPublished(ctx, matchEventID)
		}
	}
	if failedMarketID == 0 {
		failedMarketID = marketID
	}
	s.releaseLane(failedMarketID, true, false)
}

func (s *Service) runReconciler(ctx context.Context) {
	ticker := time.NewTicker(s.reconcileInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			quietCtx := logging.WithoutPGXQueryLogging(ctx)
			stats, err := s.republishUnpublishedEvents(quietCtx)
			if err != nil {
				serviceLogger.Warnf("republish unpublished settlement events failed: %v", err)
			}
			submitted, err := s.submissionRepo.ListSubmitted(quietCtx)
			if err == nil {
				stats.ResumedSubmitted = len(submitted)
				for _, record := range submitted {
					lane := s.ensureLane(record.MarketID)
					s.lanesMu.Lock()
					if lane.CurrentMatchEventID == "" {
						lane.CurrentMatchEventID = record.MatchEventID
					}
					s.lanesMu.Unlock()
					s.startWatchTask(ctx, record)
				}
			}
			queuedMarkets, err := s.submissionRepo.ListQueuedMarketIDs(quietCtx)
			if err == nil {
				stats.RequeuedMarkets = len(queuedMarkets)
				for _, marketID := range queuedMarkets {
					s.markDirty(marketID)
				}
			}
			if stats.HasWork() {
				serviceLogger.Infof("settlement reconciler processed republished_submitted=%d republished_terminal=%d resumed_submitted=%d requeued_markets=%d",
					stats.RepublishedSubmitted, stats.RepublishedTerminal, stats.ResumedSubmitted, stats.RequeuedMarkets)
			}
		}
	}
}

type reconcileStats struct {
	RepublishedSubmitted int
	RepublishedTerminal  int
	ResumedSubmitted     int
	RequeuedMarkets      int
}

func (r reconcileStats) HasWork() bool {
	return r.RepublishedSubmitted > 0 || r.RepublishedTerminal > 0 || r.ResumedSubmitted > 0 || r.RequeuedMarkets > 0
}

func (s *Service) republishUnpublishedEvents(ctx context.Context) (reconcileStats, error) {
	var stats reconcileStats
	if s == nil || s.submissionRepo == nil {
		return stats, nil
	}
	submitted, err := s.submissionRepo.ListUnpublishedSubmitted(ctx)
	if err != nil {
		return stats, fmt.Errorf("list unpublished submitted: %w", err)
	}
	for _, record := range submitted {
		if err := s.publishSubmitted(ctx, record); err != nil {
			return stats, fmt.Errorf("republish submitted match_event_id=%s: %w", record.MatchEventID, err)
		}
		if err := s.submissionRepo.MarkSubmittedEventPublished(ctx, record.MatchEventID); err != nil {
			return stats, fmt.Errorf("mark submitted published match_event_id=%s: %w", record.MatchEventID, err)
		}
		stats.RepublishedSubmitted++
		serviceLogger.Infof("republished settlement submitted event match_event_id=%s market=%d", record.MatchEventID, record.MarketID)
	}

	terminal, err := s.submissionRepo.ListUnpublishedTerminal(ctx)
	if err != nil {
		return stats, fmt.Errorf("list unpublished terminal: %w", err)
	}
	for _, record := range terminal {
		switch record.Status {
		case string(StatusConfirmed):
			if err := s.publishConfirmed(ctx, record, record.ConfirmationSlot); err != nil {
				return stats, fmt.Errorf("republish confirmed match_event_id=%s: %w", record.MatchEventID, err)
			}
		case string(StatusFailed):
			if err := s.publishFailed(ctx, record); err != nil {
				return stats, fmt.Errorf("republish failed match_event_id=%s: %w", record.MatchEventID, err)
			}
		default:
			continue
		}
		if err := s.submissionRepo.MarkTerminalEventPublished(ctx, record.MatchEventID); err != nil {
			return stats, fmt.Errorf("mark terminal published match_event_id=%s: %w", record.MatchEventID, err)
		}
		stats.RepublishedTerminal++
		serviceLogger.Infof("republished settlement terminal event match_event_id=%s status=%s market=%d", record.MatchEventID, record.Status, record.MarketID)
	}
	return stats, nil
}

func (s *Service) ensureLane(marketID uint64) *marketLane {
	s.lanesMu.Lock()
	defer s.lanesMu.Unlock()
	return s.ensureLaneLocked(marketID)
}

func (s *Service) ensureLaneLocked(marketID uint64) *marketLane {
	lane, ok := s.lanes[marketID]
	if !ok {
		lane = &marketLane{MarketID: marketID}
		s.lanes[marketID] = lane
	}
	return lane
}

func (s *Service) isLanePaused(marketID uint64) bool {
	s.lanesMu.Lock()
	defer s.lanesMu.Unlock()
	lane, ok := s.lanes[marketID]
	return ok && lane.Paused
}

func (s *Service) markDirty(marketID uint64) {
	s.dirtyMu.Lock()
	s.dirtyMarkets[marketID] = struct{}{}
	s.dirtyMu.Unlock()
	select {
	case s.dirtyWake <- struct{}{}:
	default:
	}
}

func (s *Service) drainDirtyMarkets() []uint64 {
	s.dirtyMu.Lock()
	defer s.dirtyMu.Unlock()
	out := make([]uint64, 0, len(s.dirtyMarkets))
	for marketID := range s.dirtyMarkets {
		out = append(out, marketID)
	}
	clear(s.dirtyMarkets)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Service) releaseLaneByMatchEvent(matchEventID string, paused bool) {
	s.lanesMu.Lock()
	defer s.lanesMu.Unlock()
	for _, lane := range s.lanes {
		if lane.CurrentMatchEventID != matchEventID {
			continue
		}
		lane.CurrentMatchEventID = ""
		if paused {
			lane.Paused = true
		}
		return
	}
}

func (s *Service) releaseLane(marketID uint64, paused bool, requeue bool) {
	s.lanesMu.Lock()
	lane := s.ensureLaneLocked(marketID)
	lane.CurrentMatchEventID = ""
	if paused {
		lane.Paused = true
	} else {
		lane.Paused = false
	}
	s.lanesMu.Unlock()
	if requeue {
		s.markDirty(marketID)
	}
}

func (s *Service) publishSubmitted(ctx context.Context, record SubmissionRecord) error {
	if s.client == nil {
		return nil
	}
	event := protocol.SettlementSubmittedEvent{
		EventID:              fmt.Sprintf("settlement-submitted:%s:%d", record.MatchEventID, record.RetryCount),
		SchemaVersion:        settlementSchemaNumber,
		MatchEventID:         record.MatchEventID,
		MarketID:             record.MarketID,
		MarketPDA:            record.MarketPDA,
		TxSignature:          record.TxSignature,
		RetryCount:           record.RetryCount,
		LastValidBlockHeight: record.LastValidBlockHeight,
		Wallets:              sortedWallets(record.Wallets),
		SubmittedAt:          time.Now().UTC().Unix(),
	}
	return s.client.PublishJSON(ctx, protocol.SubjectSettlementSubmittedMarket(record.MarketID), event.EventID, event)
}

func (s *Service) publishConfirmed(ctx context.Context, record SubmissionRecord, slot uint64) error {
	if s.client == nil {
		return nil
	}
	wallets := sortedWallets(record.Wallets)
	event := protocol.SettlementConfirmedEvent{
		EventID:               fmt.Sprintf("settlement-confirmed:%s", record.MatchEventID),
		SchemaVersion:         settlementSchemaNumber,
		MatchEventID:          record.MatchEventID,
		MarketID:              record.MarketID,
		MarketPDA:             record.MarketPDA,
		TxSignature:           record.TxSignature,
		SettlementTxSignature: record.TxSignature,
		RetryCount:            record.RetryCount,
		Slot:                  slot,
		Wallets:               wallets,
		ConfirmedAt:           time.Now().UTC().Unix(),
	}
	return s.client.PublishJSON(ctx, protocol.SubjectSettlementConfirmedMarket(record.MarketID), event.EventID, event)
}

func (s *Service) publishFailed(ctx context.Context, record SubmissionRecord) error {
	if s.client == nil {
		return nil
	}
	event := protocol.SettlementFailedEvent{
		EventID:       fmt.Sprintf("settlement-failed:%s", record.MatchEventID),
		SchemaVersion: settlementSchemaNumber,
		MatchEventID:  record.MatchEventID,
		MarketID:      record.MarketID,
		MarketPDA:     record.MarketPDA,
		TxSignature:   record.TxSignature,
		RetryCount:    record.RetryCount,
		ReasonCode:    record.ReasonCode,
		Wallets:       sortedWallets(record.Wallets),
		FailedAt:      time.Now().UTC().Unix(),
	}
	return s.client.PublishJSON(ctx, protocol.SubjectSettlementFailedMarket(record.MarketID), event.EventID, event)
}

func (s *Service) broadcastRawTx(ctx context.Context, rawTxBase64 string) error {
	rawTxBase64 = strings.TrimSpace(rawTxBase64)
	if rawTxBase64 == "" || s.rpc == nil {
		return nil
	}
	_, err := s.rpc.SendEncodedTransactionWithOpts(ctx, rawTxBase64, rpc.TransactionOpts{
		SkipPreflight:       false,
		PreflightCommitment: rpc.CommitmentConfirmed,
	})
	return err
}

func (s *Service) isExpired(ctx context.Context, lastValidBlockHeight uint64) (bool, error) {
	if lastValidBlockHeight == 0 || s.rpc == nil {
		return false, nil
	}
	height, err := s.rpc.GetBlockHeight(ctx, rpc.CommitmentConfirmed)
	if err != nil {
		return false, err
	}
	return height > lastValidBlockHeight, nil
}

func (s *Service) checkSignatureStatus(ctx context.Context, signature string) (chainconfirm.SignatureResult, bool, error) {
	sig, err := solana.SignatureFromBase58(strings.TrimSpace(signature))
	if err != nil {
		return chainconfirm.SignatureResult{}, false, err
	}
	resp, err := s.rpc.GetSignatureStatuses(ctx, true, sig)
	if err != nil {
		return chainconfirm.SignatureResult{}, false, err
	}
	if resp == nil || len(resp.Value) == 0 || resp.Value[0] == nil {
		return chainconfirm.SignatureResult{}, false, nil
	}
	status := resp.Value[0]
	if status.Err != nil {
		return chainconfirm.SignatureResult{
			Signature:          signature,
			Slot:               status.Slot,
			ConfirmationStatus: strings.ToLower(strings.TrimSpace(string(status.ConfirmationStatus))),
			ErrText:            fmt.Sprint(status.Err),
			ObservedAt:         time.Now().UTC(),
		}, true, nil
	}
	confirm := strings.ToLower(strings.TrimSpace(string(status.ConfirmationStatus)))
	if confirm == string(rpc.CommitmentConfirmed) || confirm == string(rpc.CommitmentFinalized) {
		return chainconfirm.SignatureResult{
			Signature:          signature,
			Slot:               status.Slot,
			ConfirmationStatus: confirm,
			ObservedAt:         time.Now().UTC(),
		}, true, nil
	}
	return chainconfirm.SignatureResult{}, false, nil
}

func (s *Service) persistObservedUserPositions(ctx context.Context, record SubmissionRecord) error {
	if s.accountRepo == nil || len(record.Wallets) == 0 {
		return nil
	}
	marketPDA, err := solana.PublicKeyFromBase58(record.MarketPDA)
	if err != nil {
		return err
	}
	records := make([]UserPositionAccountRecord, 0, len(record.Wallets))
	for _, wallet := range sortedWallets(record.Wallets) {
		userKey, err := solana.PublicKeyFromBase58(wallet)
		if err != nil {
			return err
		}
		positionPDA, err := internalsolana.DeriveUserPositionPDA(s.programID, userKey, marketPDA)
		if err != nil {
			return err
		}
		s.registry.MarkExists(record.MarketID, wallet)
		records = append(records, UserPositionAccountRecord{
			MarketID:        record.MarketID,
			WalletAddress:   wallet,
			UserPositionPDA: positionPDA.String(),
		})
	}
	return s.accountRepo.UpsertObserved(ctx, records)
}

func sortedWallets(wallets []string) []string {
	uniq := make(map[string]struct{}, len(wallets))
	out := make([]string, 0, len(wallets))
	for _, wallet := range wallets {
		wallet = strings.TrimSpace(wallet)
		if wallet == "" {
			continue
		}
		if _, ok := uniq[wallet]; ok {
			continue
		}
		uniq[wallet] = struct{}{}
		out = append(out, wallet)
	}
	sort.Strings(out)
	return out
}

func isDeterministicSettlementError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	keywords := []string{
		"simulation failed",
		"instructionerror",
		"custom program error",
		"invalid account",
		"accountownedbywrongprogram",
		"already in use",
		"program failed",
	}
	for _, keyword := range keywords {
		if strings.Contains(msg, keyword) {
			return true
		}
	}
	return false
}
