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
	defaultConsumerName          = "settlement-execution"
	catchUpBatch                 = 32
	runBatch                     = 16
	defaultPrepareWorkerTick     = 300 * time.Millisecond
	defaultSubmittedStatusPoll   = 15 * time.Second
	defaultSubmittedRebroadcast  = 8 * time.Second
	defaultSubmittedBlockPoll    = 15 * time.Second
	defaultTerminalPollInterval  = 12 * time.Second
	defaultTerminalPollBatchSize = 128
	defaultBlockhashPollInterval = 15 * time.Second
	defaultBlockhashMaxCacheAge  = 45 * time.Second
	defaultReconcileInterval     = 10 * time.Second
	defaultSubmitStepTimeout     = 15 * time.Second
	defaultSchedulerFallbackScan = 300 * time.Millisecond
	defaultSendSkipPreflight     = true
	settlementSchemaNumber       = 1
)

type ServiceOptions struct {
	PrepareWorkerTick     time.Duration
	SchedulerFallbackScan time.Duration
	SubmittedStatusPoll   time.Duration
	SubmittedRebroadcast  time.Duration
	SubmittedBlockPoll    time.Duration
	TerminalPollInterval  time.Duration
	TerminalPollBatchSize int
	BlockhashPollInterval time.Duration
	BlockhashMaxCacheAge  time.Duration
	SendSkipPreflight     *bool
	MaxTxBytes            int
	AddressTables         map[solana.PublicKey]solana.PublicKeySlice
}

func (o ServiceOptions) withDefaults() ServiceOptions {
	if o.PrepareWorkerTick <= 0 {
		o.PrepareWorkerTick = defaultPrepareWorkerTick
	}
	if o.SchedulerFallbackScan <= 0 {
		o.SchedulerFallbackScan = defaultSchedulerFallbackScan
	}
	if o.SubmittedStatusPoll <= 0 {
		o.SubmittedStatusPoll = defaultSubmittedStatusPoll
	}
	if o.SubmittedRebroadcast <= 0 {
		o.SubmittedRebroadcast = defaultSubmittedRebroadcast
	}
	if o.SubmittedBlockPoll <= 0 {
		o.SubmittedBlockPoll = defaultSubmittedBlockPoll
	}
	if o.TerminalPollInterval <= 0 {
		o.TerminalPollInterval = defaultTerminalPollInterval
	}
	if o.TerminalPollBatchSize <= 0 {
		o.TerminalPollBatchSize = defaultTerminalPollBatchSize
	}
	if o.BlockhashPollInterval <= 0 {
		o.BlockhashPollInterval = defaultBlockhashPollInterval
	}
	if o.BlockhashMaxCacheAge <= 0 {
		o.BlockhashMaxCacheAge = defaultBlockhashMaxCacheAge
	}
	if o.SendSkipPreflight == nil {
		value := defaultSendSkipPreflight
		o.SendSkipPreflight = &value
	}
	if o.MaxTxBytes <= 0 {
		o.MaxTxBytes = internalsolana.DefaultSettlementMaxTxBytes
	}
	return o
}

type ProcessedSettlementObserver interface {
	ObserveProcessedSettlement(marketID uint64, wallets []string, matchEventJSON []byte)
}

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
	orderRegistry  *OrderStateRegistry
	accountRepo    *UserPositionAccountRepo
	orderRepo      *OrderStateAccountRepo
	submitter      *Submitter
	submissionRepo *submissionRepo
	programID      solana.PublicKey
	blockhashes    *BlockhashManager
	txEstimator    *internalsolana.SettlementTxEstimator
	maxTxBytes     int

	lanesMu      sync.Mutex
	lanes        map[uint64]*marketLane
	dirtyMu      sync.Mutex
	dirtyMarkets map[uint64]struct{}
	dirtyWake    chan struct{}
	prepareMu    sync.Mutex
	prepareDirty map[uint64]struct{}
	prepareWake  chan struct{}

	watchMu      sync.Mutex
	watchCancels map[string]context.CancelFunc

	reconcileInterval time.Duration
	prepareWorkerTick time.Duration
	schedulerScan     time.Duration
	submittedPoll     time.Duration
	rebroadcast       time.Duration
	submittedHeight   time.Duration
	terminalPoll      time.Duration
	terminalBatchSize int
	sendSkipPreflight bool
	processedObserver ProcessedSettlementObserver
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
	options ServiceOptions,
) *Service {
	if consumerName == "" {
		consumerName = defaultConsumerName
	}
	if reconcileInterval <= 0 {
		reconcileInterval = defaultReconcileInterval
	}
	options = options.withDefaults()
	rpcClient := logging.NewSolanaRPCClient("settlement-rpc", rpcURL)
	addressTables := internalsolana.CopyAddressTables(options.AddressTables)
	return &Service{
		consumerName:      consumerName,
		client:            client,
		pool:              pool,
		rpc:               rpcClient,
		router:            router,
		registry:          NewUserPositionRegistry(),
		orderRegistry:     NewOrderStateRegistry(),
		accountRepo:       NewUserPositionAccountRepo(pool),
		orderRepo:         NewOrderStateAccountRepo(pool),
		submitter:         &Submitter{ProgramID: programID, Relayer: relayer, RPC: rpcClient, AddressTables: addressTables},
		submissionRepo:    newSubmissionRepo(pool),
		programID:         programID,
		blockhashes:       NewBlockhashManager(rpcClient, rpc.CommitmentProcessed, options.BlockhashPollInterval, options.BlockhashMaxCacheAge),
		txEstimator:       internalsolana.NewSettlementTxEstimator(programID, addressTables),
		maxTxBytes:        options.MaxTxBytes,
		lanes:             make(map[uint64]*marketLane),
		dirtyMarkets:      make(map[uint64]struct{}),
		dirtyWake:         make(chan struct{}, 1),
		prepareDirty:      make(map[uint64]struct{}),
		prepareWake:       make(chan struct{}, 1),
		watchCancels:      make(map[string]context.CancelFunc),
		reconcileInterval: reconcileInterval,
		prepareWorkerTick: options.PrepareWorkerTick,
		schedulerScan:     options.SchedulerFallbackScan,
		submittedPoll:     options.SubmittedStatusPoll,
		rebroadcast:       options.SubmittedRebroadcast,
		submittedHeight:   options.SubmittedBlockPoll,
		terminalPoll:      options.TerminalPollInterval,
		terminalBatchSize: options.TerminalPollBatchSize,
		sendSkipPreflight: *options.SendSkipPreflight,
	}
}

func (s *Service) Start(ctx context.Context) error {
	if s == nil || s.client == nil || s.submitter == nil || s.submissionRepo == nil {
		return nil
	}
	if err := LoadRegistryFromRepo(ctx, s.accountRepo, s.registry); err != nil {
		return fmt.Errorf("load user position registry: %w", err)
	}
	if err := LoadOrderStateRegistryFromRepo(ctx, s.orderRepo, s.orderRegistry); err != nil {
		return fmt.Errorf("load order state registry: %w", err)
	}
	if err := s.ensureSubscription(); err != nil {
		return err
	}
	if err := s.recoverState(ctx); err != nil {
		return err
	}
	s.blockhashes.Start(ctx)
	go s.runIngress(ctx)
	go s.runPrepareWorker(ctx)
	go s.runScheduler(ctx)
	go s.runTerminalPoller(ctx)
	go s.runReconciler(ctx)
	return nil
}

func (s *Service) SetProcessedSettlementObserver(observer ProcessedSettlementObserver) {
	if s == nil {
		return
	}
	s.processedObserver = observer
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
	processed, err := s.submissionRepo.ListProcessed(quietCtx, 100000)
	if err != nil {
		return fmt.Errorf("settlement load processed rows: %w", err)
	}
	for _, record := range processed {
		s.observeProcessedEvidence(ctx, record)
	}
	submitted, err := s.submissionRepo.ListSubmitted(quietCtx)
	if err != nil {
		return fmt.Errorf("settlement load submitted rows: %w", err)
	}
	for _, record := range submitted {
		fastForwarded := s.tryFastForwardSubmitted(ctx, record)
		if fastForwarded {
			continue
		}
		lane := s.ensureLane(record.MarketID)
		lane.CurrentMatchEventID = record.MatchEventID
		s.startWatchTask(ctx, record)
	}
	preparedMarkets, err := s.submissionRepo.ListPreparedMarketIDs(quietCtx)
	if err != nil {
		return fmt.Errorf("settlement load prepared markets: %w", err)
	}
	for _, marketID := range preparedMarkets {
		s.markDirty(marketID)
	}
	queuedMarkets, err := s.submissionRepo.ListQueuedMarketIDs(quietCtx)
	if err != nil {
		return fmt.Errorf("settlement load queued markets: %w", err)
	}
	for _, marketID := range queuedMarkets {
		s.markPrepareDirty(marketID)
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
			if item.insertRow.MarketLaneStatus == string(LaneActive) {
				s.markPrepareDirty(item.event.MarketID)
			}
		}
		_ = item.msg.Ack()
	}
}

func (s *Service) runPrepareWorker(ctx context.Context) {
	ticker := time.NewTicker(s.prepareWorkerTick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.prepareWake:
			s.prepareDirtyMarkets(ctx)
		case <-ticker.C:
			s.prepareDirtyMarkets(ctx)
		}
	}
}

func (s *Service) prepareDirtyMarkets(ctx context.Context) {
	quietCtx := logging.WithoutPGXQueryLogging(ctx)
	for _, marketID := range s.drainPrepareDirtyMarkets() {
		record, ok, err := s.submissionRepo.LoadNextQueuedByMarket(quietCtx, marketID)
		if err != nil {
			serviceLogger.Warnf("prepare worker load queued failed market=%d err=%v", marketID, err)
			s.schedulePrepareRetry(marketID)
			continue
		}
		if !ok {
			continue
		}
		s.prepareSubmission(quietCtx, record)
	}
}

func (s *Service) prepareSubmission(ctx context.Context, record SubmissionRecord) {
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(record.MatchEventJSON, &event); err != nil {
		serviceLogger.Warnf("prepare worker decode event failed match_event_id=%s err=%v", record.MatchEventID, err)
		s.schedulePrepareRetry(record.MarketID)
		return
	}
	batch, err := BuildSubmissionBatch(event, BuildConfig{ProgramID: s.programID})
	if err != nil {
		serviceLogger.Warnf("prepare worker build batch failed match_event_id=%s err=%v", record.MatchEventID, err)
		s.schedulePrepareRetry(record.MarketID)
		return
	}
	s.applyWarmOrderStates(&batch)
	if err := s.enforceEstimatedTxBytes(ctx, record, batch); err != nil {
		serviceLogger.Warnf("prepare worker estimate failed match_event_id=%s err=%v", record.MatchEventID, err)
		s.schedulePrepareRetry(record.MarketID)
		return
	}
	preparedPayload, err := encodePreparedPayload(batch)
	if err != nil {
		serviceLogger.Warnf("prepare worker encode payload failed match_event_id=%s err=%v", record.MatchEventID, err)
		s.schedulePrepareRetry(record.MarketID)
		return
	}
	applied, err := s.submissionRepo.MarkPreparedCAS(ctx, record.MatchEventID, preparedPayload)
	if err != nil {
		serviceLogger.Warnf("prepare worker mark prepared failed match_event_id=%s err=%v", record.MatchEventID, err)
		s.schedulePrepareRetry(record.MarketID)
		return
	}
	if !applied {
		return
	}
	s.markDirty(record.MarketID)
	s.markPrepareDirty(record.MarketID)
}

func (s *Service) runScheduler(ctx context.Context) {
	ticker := time.NewTicker(s.schedulerScan)
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

	record, ok, err := s.submissionRepo.LoadNextPreparedByMarket(ctx, marketID)
	if err != nil {
		serviceLogger.Warnf("load prepared settlement row failed market=%d err=%v", marketID, err)
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
	if record.Status != string(StatusPrepared) {
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(record.MarketID)
		return
	}
	batch, err := decodePreparedPayload(record.PreparedPayload, BuildConfig{ProgramID: s.programID})
	if err != nil {
		serviceLogger.Warnf("decode prepared settlement payload failed match_event_id=%s err=%v", matchEventID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		return
	}
	serviceLogger.Infof("settlement submit started match_event_id=%s market=%d wallets=%d fills=%d", matchEventID, batch.MarketID, len(record.Wallets), len(batch.Fills))
	signCtx, cancel := chainconfirm.WithTimeout(ctx, defaultSubmitStepTimeout)
	defer cancel()
	snapshot, err := s.blockhashes.Snapshot(signCtx)
	if err != nil {
		serviceLogger.Warnf("read blockhash cache failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	tx, err := s.submitter.BuildTransactionWithBlockhash(batch, snapshot.Blockhash)
	if err != nil {
		serviceLogger.Warnf("build settlement transaction failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	sig, rawTx, err := s.submitter.SignTransaction(tx)
	if err != nil {
		serviceLogger.Warnf("sign settlement tx failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, err)
		s.releaseLaneByMatchEvent(matchEventID, false)
		s.markDirty(batch.MarketID)
		return
	}
	if s.maxTxBytes > 0 {
		wireBytes, sizeErr := s.submitter.TransactionWireBytes(tx)
		if sizeErr != nil {
			serviceLogger.Warnf("measure settlement tx bytes failed match_event_id=%s market=%d err=%v", matchEventID, batch.MarketID, sizeErr)
			s.releaseLaneByMatchEvent(matchEventID, false)
			s.markDirty(batch.MarketID)
			return
		}
		if wireBytes > s.maxTxBytes {
			serviceLogger.Warnf("settlement tx exceeds byte limit match_event_id=%s market=%d bytes=%d limit=%d", matchEventID, batch.MarketID, wireBytes, s.maxTxBytes)
			s.failBeforeSubmit(ctx, record, "tx_bytes_exceeded")
			return
		}
	}
	serviceLogger.Infof("settlement tx signed match_event_id=%s market=%d sig=%s last_valid_block_height=%d", matchEventID, batch.MarketID, sig.String(), snapshot.LastValidBlockHeight)
	applied, err := s.submissionRepo.MarkSubmittedCAS(ctx, matchEventID, sig.String(), rawTx, snapshot.LastValidBlockHeight)
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
			s.failSubmission(ctx, matchEventID, batch.MarketID, sig.String(), "simulation_failed", true)
			return
		}
	} else {
		serviceLogger.Infof("settlement raw tx broadcast match_event_id=%s market=%d sig=%s", matchEventID, batch.MarketID, sig.String())
	}
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
		status, observed, err := s.checkSignatureProcessed(ctx, current.TxSignature)
		if err == nil && observed {
			if status.ErrText != "" {
				s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed", true)
			} else {
				s.processSubmission(ctx, current, status.Slot)
			}
			return
		}

		var (
			routerCh    chan chainconfirm.SignatureResult
			unsubscribe func()
		)
		if s.router != nil {
			routerCh = make(chan chainconfirm.SignatureResult, 1)
			unsub, subErr := s.router.SubscribeSignature(current.TxSignature, "settlement:"+current.MatchEventID, "settlement", "processed", routerCh)
			if subErr == nil {
				unsubscribe = unsub
			} else {
				serviceLogger.Warnf("settlement router subscribe failed match_event_id=%s sig=%s err=%v", current.MatchEventID, current.TxSignature, subErr)
			}
		}
		statusTicker := time.NewTicker(s.submittedPoll)
		rebroadcastTicker := time.NewTicker(s.rebroadcast)
		heightTicker := time.NewTicker(s.submittedHeight)
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
					s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed", true)
				} else {
					s.processSubmission(ctx, current, res.Slot)
				}
				return
			case <-statusTicker.C:
				status, observed, err := s.checkSignatureProcessed(ctx, current.TxSignature)
				if err != nil || !observed {
					continue
				}
				if unsubscribe != nil {
					unsubscribe()
				}
				statusTicker.Stop()
				rebroadcastTicker.Stop()
				heightTicker.Stop()
				if status.ErrText != "" {
					s.failSubmission(ctx, current.MatchEventID, current.MarketID, current.TxSignature, "chain_execution_failed", true)
				} else {
					s.processSubmission(ctx, current, status.Slot)
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
	batch, err := decodePreparedPayload(current.PreparedPayload, BuildConfig{ProgramID: s.programID})
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	snapshot, err := s.blockhashes.ForceRefresh(ctx)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	tx, err := s.submitter.BuildTransactionWithBlockhash(batch, snapshot.Blockhash)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	sig, rawTx, err := s.submitter.SignTransaction(tx)
	if err != nil {
		return SubmissionRecord{}, false, err
	}
	replaced, err := s.submissionRepo.ReplaceSignatureCAS(ctx, current.MatchEventID, current.TxSignature, sig.String(), rawTx, snapshot.LastValidBlockHeight)
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
			s.failSubmission(ctx, current.MatchEventID, current.MarketID, sig.String(), "simulation_failed", true)
		}
	}
	return latest, true, nil
}

func (s *Service) processSubmission(ctx context.Context, record SubmissionRecord, slot uint64) {
	applied, err := s.submissionRepo.MarkProcessedCAS(ctx, record.MatchEventID, record.TxSignature, slot)
	if err != nil {
		serviceLogger.Warnf("mark settlement processed failed match_event_id=%s err=%v", record.MatchEventID, err)
		return
	}
	if !applied {
		return
	}
	latest, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, record.MatchEventID)
	if err == nil && ok {
		record = latest
	}
	s.observeProcessedEvidence(ctx, record)
	s.lanesMu.Lock()
	lane := s.ensureLaneLocked(record.MarketID)
	paused := lane.Paused
	lane.CurrentMatchEventID = ""
	s.lanesMu.Unlock()
	if !paused {
		s.markDirty(record.MarketID)
	}
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
	latest, ok, err := s.submissionRepo.LoadByMatchEventID(ctx, record.MatchEventID)
	if err == nil && ok {
		record = latest
	}
	if err := s.publishConfirmed(ctx, record, slot); err != nil {
		serviceLogger.Warnf("publish settlement confirmed failed match_event_id=%s err=%v", record.MatchEventID, err)
	} else {
		_ = s.submissionRepo.MarkTerminalEventPublished(ctx, record.MatchEventID)
	}
}

func (s *Service) failSubmission(ctx context.Context, matchEventID string, marketID uint64, txSignature string, reasonCode string, releaseCurrent bool) {
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
	if releaseCurrent {
		s.releaseLane(failedMarketID, true, false)
		return
	}
	s.pauseLane(failedMarketID)
}

func (s *Service) failBeforeSubmit(ctx context.Context, record SubmissionRecord, reasonCode string) {
	applied, failedMarketID, err := s.submissionRepo.MarkFailedBeforeSubmitAndPauseQueued(ctx, record.MatchEventID, reasonCode)
	if err != nil {
		serviceLogger.Warnf("mark settlement pre-submit failed match_event_id=%s err=%v", record.MatchEventID, err)
		return
	}
	if !applied {
		return
	}
	latest, ok, loadErr := s.submissionRepo.LoadByMatchEventID(ctx, record.MatchEventID)
	if loadErr == nil && ok {
		record = latest
	}
	if err := s.publishFailed(ctx, record); err != nil {
		serviceLogger.Warnf("publish settlement pre-submit failed event failed match_event_id=%s err=%v", record.MatchEventID, err)
	} else {
		_ = s.submissionRepo.MarkTerminalEventPublished(ctx, record.MatchEventID)
	}
	if failedMarketID == 0 {
		failedMarketID = record.MarketID
	}
	s.pauseLane(failedMarketID)
	s.releaseLaneByMatchEvent(record.MatchEventID, true)
}

func (s *Service) observeProcessedEvidence(ctx context.Context, record SubmissionRecord) {
	if err := s.persistObservedUserPositions(ctx, record); err != nil {
		serviceLogger.Warnf("persist settlement user positions failed match_event_id=%s err=%v", record.MatchEventID, err)
	}
	if err := s.persistObservedOrderStates(ctx, record); err != nil {
		serviceLogger.Warnf("persist settlement order states failed match_event_id=%s err=%v", record.MatchEventID, err)
	}
	if s.processedObserver != nil {
		s.processedObserver.ObserveProcessedSettlement(record.MarketID, sortedWallets(record.Wallets), record.MatchEventJSON)
	}
}

func (s *Service) enforceEstimatedTxBytes(ctx context.Context, record SubmissionRecord, batch SubmissionBatch) error {
	if s == nil || s.txEstimator == nil || s.maxTxBytes <= 0 {
		return nil
	}
	estimate, err := s.txEstimator.Estimate(settlementTxShapeForBatch(batch))
	if err != nil {
		return err
	}
	if estimate.TransactionBytes <= s.maxTxBytes {
		return nil
	}
	serviceLogger.Warnf("prepared settlement batch exceeds estimated byte limit match_event_id=%s market=%d bytes=%d limit=%d versioned=%t lookups=%d",
		record.MatchEventID, record.MarketID, estimate.TransactionBytes, s.maxTxBytes, estimate.Versioned, estimate.LookupAccounts)
	s.failBeforeSubmit(ctx, record, "tx_bytes_exceeded")
	return nil
}

func (s *Service) tryFastForwardSubmitted(ctx context.Context, record SubmissionRecord) bool {
	status, observed, err := s.checkSignatureProcessed(ctx, record.TxSignature)
	if err != nil || !observed {
		return false
	}
	if status.ErrText != "" {
		s.failSubmission(ctx, record.MatchEventID, record.MarketID, record.TxSignature, "chain_execution_failed", true)
		return true
	}
	s.processSubmission(ctx, record, status.Slot)
	return true
}

func (s *Service) runTerminalPoller(ctx context.Context) {
	ticker := time.NewTicker(s.terminalPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.processTerminalBatch(ctx)
		}
	}
}

func (s *Service) processTerminalBatch(ctx context.Context) {
	records, err := s.submissionRepo.ListProcessed(ctx, s.terminalBatchSize)
	if err != nil {
		serviceLogger.Warnf("terminal poller list processed failed: %v", err)
		return
	}
	for _, record := range records {
		status, terminal, statusErr := s.checkSignatureStatus(ctx, record.TxSignature)
		if statusErr != nil || !terminal {
			continue
		}
		if status.ErrText != "" {
			s.failSubmission(ctx, record.MatchEventID, record.MarketID, record.TxSignature, "chain_execution_failed", false)
			continue
		}
		s.confirmSubmission(ctx, record, status.Slot)
	}
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
					if s.tryFastForwardSubmitted(ctx, record) {
						continue
					}
					lane := s.ensureLane(record.MarketID)
					s.lanesMu.Lock()
					if lane.CurrentMatchEventID == "" {
						lane.CurrentMatchEventID = record.MatchEventID
					}
					s.lanesMu.Unlock()
					s.startWatchTask(ctx, record)
				}
			}
			preparedMarkets, err := s.submissionRepo.ListPreparedMarketIDs(quietCtx)
			if err == nil {
				stats.RequeuedMarkets = len(preparedMarkets)
				for _, marketID := range preparedMarkets {
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

func (s *Service) markPrepareDirty(marketID uint64) {
	s.prepareMu.Lock()
	s.prepareDirty[marketID] = struct{}{}
	s.prepareMu.Unlock()
	select {
	case s.prepareWake <- struct{}{}:
	default:
	}
}

func (s *Service) drainPrepareDirtyMarkets() []uint64 {
	s.prepareMu.Lock()
	defer s.prepareMu.Unlock()
	out := make([]uint64, 0, len(s.prepareDirty))
	for marketID := range s.prepareDirty {
		out = append(out, marketID)
	}
	clear(s.prepareDirty)
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func (s *Service) schedulePrepareRetry(marketID uint64) {
	delay := s.prepareWorkerTick
	if delay <= 0 {
		delay = defaultPrepareWorkerTick
	}
	time.AfterFunc(delay, func() {
		s.markPrepareDirty(marketID)
	})
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

func (s *Service) pauseLane(marketID uint64) {
	s.lanesMu.Lock()
	lane := s.ensureLaneLocked(marketID)
	lane.Paused = true
	s.lanesMu.Unlock()
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
		SkipPreflight:       s.sendSkipPreflight,
		PreflightCommitment: rpc.CommitmentProcessed,
	})
	return err
}

func (s *Service) isExpired(ctx context.Context, lastValidBlockHeight uint64) (bool, error) {
	if lastValidBlockHeight == 0 || s.rpc == nil {
		return false, nil
	}
	height, err := s.rpc.GetBlockHeight(ctx, rpc.CommitmentProcessed)
	if err != nil {
		return false, err
	}
	return height > lastValidBlockHeight, nil
}

func (s *Service) checkSignatureProcessed(ctx context.Context, signature string) (chainconfirm.SignatureResult, bool, error) {
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
	confirm := strings.ToLower(strings.TrimSpace(string(status.ConfirmationStatus)))
	if status.Err != nil {
		return chainconfirm.SignatureResult{
			Signature:          signature,
			Slot:               status.Slot,
			ConfirmationStatus: confirm,
			ErrText:            fmt.Sprint(status.Err),
			ObservedAt:         time.Now().UTC(),
		}, true, nil
	}
	if confirm == string(rpc.CommitmentProcessed) || confirm == string(rpc.CommitmentConfirmed) || confirm == string(rpc.CommitmentFinalized) {
		return chainconfirm.SignatureResult{
			Signature:          signature,
			Slot:               status.Slot,
			ConfirmationStatus: confirm,
			ObservedAt:         time.Now().UTC(),
		}, true, nil
	}
	return chainconfirm.SignatureResult{}, false, nil
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

func (s *Service) persistObservedOrderStates(ctx context.Context, record SubmissionRecord) error {
	if s.orderRepo == nil || len(record.MatchEventJSON) == 0 {
		s.markObservedOrderStates(record)
		return nil
	}
	marketPDA, err := solana.PublicKeyFromBase58(record.MarketPDA)
	if err != nil {
		return err
	}
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(record.MatchEventJSON, &event); err != nil {
		return err
	}
	records := make([]OrderStateAccountRecord, 0, len(event.Orders))
	seen := make(map[OrderStateKey]struct{}, len(event.Orders))
	for _, order := range event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		nonce := order.Execution.Nonce
		key := OrderStateKey{MarketID: record.MarketID, Wallet: wallet, Nonce: nonce}
		if wallet == "" || nonce == 0 {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		userKey, err := solana.PublicKeyFromBase58(wallet)
		if err != nil {
			return err
		}
		orderStatePDA, err := internalsolana.DeriveOrderStatePDA(s.programID, userKey, marketPDA, nonce)
		if err != nil {
			return err
		}
		s.orderRegistry.MarkExists(record.MarketID, wallet, nonce)
		records = append(records, OrderStateAccountRecord{
			MarketID:         record.MarketID,
			WalletAddress:    wallet,
			Nonce:            nonce,
			OrderStatePDA:    orderStatePDA.String(),
			CreatedByRelayer: s.submitter.Relayer.PublicKey().String(),
			CreatedTxSig:     record.TxSignature,
		})
	}
	return s.orderRepo.UpsertObserved(ctx, records)
}

func settlementTxShapeForBatch(batch SubmissionBatch) internalsolana.SettlementTxShape {
	coldOrders := 0
	for _, order := range batch.Orders {
		if !order.Warm {
			coldOrders++
		}
	}
	return internalsolana.SettlementTxShape{
		UniqueUsers: len(batch.UniqueUsers),
		Orders:      len(batch.Orders),
		ColdOrders:  coldOrders,
		Fills:       len(batch.Fills),
	}
}

func (s *Service) applyWarmOrderStates(batch *SubmissionBatch) {
	if s == nil || s.orderRegistry == nil || batch == nil {
		return
	}
	for idx := range batch.Orders {
		order := &batch.Orders[idx]
		order.Warm = s.orderRegistry.Has(batch.MarketID, order.Intent.User.String(), order.Intent.Nonce)
	}
}

func (s *Service) markObservedOrderStates(record SubmissionRecord) {
	if s == nil || s.orderRegistry == nil || len(record.MatchEventJSON) == 0 {
		return
	}
	var event matching.MatchBatchEvent
	if err := json.Unmarshal(record.MatchEventJSON, &event); err != nil {
		return
	}
	for _, order := range event.Orders {
		wallet := strings.TrimSpace(order.Execution.WalletAddress)
		if wallet == "" || order.Execution.Nonce == 0 {
			continue
		}
		s.orderRegistry.MarkExists(record.MarketID, wallet, order.Execution.Nonce)
	}
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
