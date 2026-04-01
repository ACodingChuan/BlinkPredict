package bootstrap

import (
	"context"
	"sync/atomic"
	"time"

	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/pusher"
	"blinkpredict/banckend/internal/settlement"
	"blinkpredict/banckend/internal/writer"
)

var logger = logging.New("bootstrap")

type Coordinator struct {
	writer          *writer.Writer
	matcher         *matching.MarketManager
	pusher          *pusher.Service
	settlement      *settlement.Service
	writerState     atomic.Int32
	matcherState    atomic.Int32
	pusherState     atomic.Int32
	settlementState atomic.Int32
	writeReady      atomic.Bool
	tickInterval    time.Duration
}

type ModuleState string

const (
	StateInit       ModuleState = "init"
	StateStarting   ModuleState = "starting"
	StateCatchingUp ModuleState = "catching_up"
	StateRecovering ModuleState = "recovering"
	StateReady      ModuleState = "ready"
	StateFailed     ModuleState = "failed"
)

func NewCoordinator(writer *writer.Writer, matcher *matching.MarketManager, pusher *pusher.Service, settlementSvc *settlement.Service, tickInterval time.Duration) *Coordinator {
	return &Coordinator{
		writer:       writer,
		matcher:      matcher,
		pusher:       pusher,
		settlement:   settlementSvc,
		tickInterval: tickInterval,
	}
}

func (c *Coordinator) Start(ctx context.Context) error {
	if c.writer != nil {
		c.writerState.Store(int32(StateCatchingUpIndex()))
		logger.Infof("starting writer catch-up")
		if err := c.writer.Start(ctx); err != nil {
			c.writerState.Store(int32(StateFailedIndex()))
			return err
		}
		c.writerState.Store(int32(StateReadyIndex()))
	}
	if c.matcher != nil {
		c.matcherState.Store(int32(StateRecoveringIndex()))
		logger.Infof("recovering matcher from orders")
		if err := c.matcher.RecoverFromStore(ctx); err != nil {
			c.matcherState.Store(int32(StateFailedIndex()))
			return err
		}
		logger.Infof("running bootstrap tick")
		if err := c.matcher.RunBootstrapTick(ctx); err != nil {
			c.matcherState.Store(int32(StateFailedIndex()))
			return err
		}
		go func() {
			if err := c.matcher.StartConsumer(ctx); err != nil {
				logger.Warnf("matcher consumer stopped: %v", err)
			}
		}()
		c.matcher.StartTickLoop(ctx, c.tickInterval)
		c.matcherState.Store(int32(StateReadyIndex()))
	}
	if c.pusher != nil {
		c.pusherState.Store(int32(StateStartingIndex()))
		logger.Infof("starting pusher service")
		if err := c.pusher.Start(ctx); err != nil {
			c.pusherState.Store(int32(StateFailedIndex()))
			return err
		}
		c.pusherState.Store(int32(StateReadyIndex()))
	}
	if c.settlement != nil {
		c.settlementState.Store(int32(StateStartingIndex()))
		logger.Infof("starting settlement service")
		if err := c.settlement.Start(ctx); err != nil {
			c.settlementState.Store(int32(StateFailedIndex()))
			return err
		}
		c.settlementState.Store(int32(StateReadyIndex()))
	}
	c.writeReady.Store(true)
	logger.Infof("bootstrap ready; write traffic enabled")
	return nil
}

func (c *Coordinator) OrdersReady() bool {
	return c.writeReady.Load()
}

func (c *Coordinator) Status() map[string]any {
	return map[string]any{
		"writer":              stateFromIndex(c.writerState.Load()),
		"matcher":             stateFromIndex(c.matcherState.Load()),
		"pusher":              stateFromIndex(c.pusherState.Load()),
		"settlement":          stateFromIndex(c.settlementState.Load()),
		"gateway_write_ready": c.writeReady.Load(),
	}
}

func StateInitIndex() int {
	return 0
}

func StateStartingIndex() int {
	return 1
}

func StateCatchingUpIndex() int {
	return 2
}

func StateRecoveringIndex() int {
	return 3
}

func StateReadyIndex() int {
	return 4
}

func StateFailedIndex() int {
	return 5
}

func stateFromIndex(v int32) ModuleState {
	switch v {
	case int32(StateStartingIndex()):
		return StateStarting
	case int32(StateCatchingUpIndex()):
		return StateCatchingUp
	case int32(StateRecoveringIndex()):
		return StateRecovering
	case int32(StateReadyIndex()):
		return StateReady
	case int32(StateFailedIndex()):
		return StateFailed
	default:
		return StateInit
	}
}
