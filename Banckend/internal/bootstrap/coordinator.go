package bootstrap

import (
	"context"
	"sync/atomic"
	"time"

	"blinkpredict/banckend/internal/depositconfirm"
	"blinkpredict/banckend/internal/funds"
	"blinkpredict/banckend/internal/logging"
	"blinkpredict/banckend/internal/marketconfirm"
	"blinkpredict/banckend/internal/matching"
	"blinkpredict/banckend/internal/pusher"
	"blinkpredict/banckend/internal/settlement"
	"blinkpredict/banckend/internal/writer"
)

var logger = logging.New("bootstrap")

type Coordinator struct {
	writer             *writer.Writer
	funds              *funds.Service
	matcher            *matching.MarketManager
	pusher             *pusher.Service
	depositConfirm     *depositconfirm.Service
	marketConfirm      *marketconfirm.Service
	marketProjector    *marketconfirm.Projector
	settlement         *settlement.Service
	writerState        atomic.Int32
	fundsState         atomic.Int32
	matcherState       atomic.Int32
	pusherState        atomic.Int32
	depositState       atomic.Int32
	marketConfirmState atomic.Int32
	projectorState     atomic.Int32
	settlementState    atomic.Int32
	writeReady         atomic.Bool
	tickInterval       time.Duration
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

func NewCoordinator(
	writerSvc *writer.Writer,
	fundsSvc *funds.Service,
	matcher *matching.MarketManager,
	pusherSvc *pusher.Service,
	depositSvc *depositconfirm.Service,
	marketConfirmSvc *marketconfirm.Service,
	marketProjector *marketconfirm.Projector,
	settlementSvc *settlement.Service,
	tickInterval time.Duration,
) *Coordinator {
	return &Coordinator{
		writer:          writerSvc,
		funds:           fundsSvc,
		matcher:         matcher,
		pusher:          pusherSvc,
		depositConfirm:  depositSvc,
		marketConfirm:   marketConfirmSvc,
		marketProjector: marketProjector,
		settlement:      settlementSvc,
		tickInterval:    tickInterval,
	}
}

func (c *Coordinator) Start(ctx context.Context) error {
	if c.writer != nil {
		if err := c.startModule(&c.writerState, StateCatchingUpIndex(), "writer", "starting writer catch-up", func() error {
			return c.writer.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.funds != nil {
		if err := c.startModule(&c.fundsState, StateRecoveringIndex(), "funds", "recovering funds snapshots", func() error {
			return c.funds.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.matcher != nil {
		if err := c.startModule(&c.matcherState, StateRecoveringIndex(), "matcher", "recovering matcher from orders", func() error {
			return c.matcher.Start(ctx, c.tickInterval)
		}); err != nil {
			return err
		}
	}
	if c.pusher != nil {
		if err := c.startModule(&c.pusherState, StateStartingIndex(), "pusher", "starting pusher service", func() error {
			return c.pusher.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.depositConfirm != nil {
		if err := c.startModule(&c.depositState, StateStartingIndex(), "deposit-confirm", "starting deposit confirm service", func() error {
			return c.depositConfirm.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.marketConfirm != nil {
		if err := c.startModule(&c.marketConfirmState, StateStartingIndex(), "market-confirm", "starting market confirm service", func() error {
			return c.marketConfirm.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.marketProjector != nil {
		if err := c.startModule(&c.projectorState, StateStartingIndex(), "market-projector", "starting market projector", func() error {
			return c.marketProjector.Start(ctx)
		}); err != nil {
			return err
		}
	}
	if c.settlement != nil {
		if err := c.startModule(&c.settlementState, StateStartingIndex(), "settlement", "starting settlement service", func() error {
			return c.settlement.Start(ctx)
		}); err != nil {
			return err
		}
	}
	c.writeReady.Store(true)
	logger.Infof("bootstrap ready; write traffic enabled")
	return nil
}

func (c *Coordinator) startModule(state *atomic.Int32, startState int, module string, message string, startFn func() error) error {
	if startFn == nil {
		return nil
	}
	state.Store(int32(startState))
	logger.Infof("%s", message)
	if err := startFn(); err != nil {
		state.Store(int32(StateFailedIndex()))
		logger.Warnf("%s start failed: %v", module, err)
		return err
	}
	state.Store(int32(StateReadyIndex()))
	return nil
}

func (c *Coordinator) OrdersReady() bool {
	return c.writeReady.Load()
}

func (c *Coordinator) Status() map[string]any {
	return map[string]any{
		"writer":              stateFromIndex(c.writerState.Load()),
		"funds":               stateFromIndex(c.fundsState.Load()),
		"matcher":             stateFromIndex(c.matcherState.Load()),
		"pusher":              stateFromIndex(c.pusherState.Load()),
		"deposit-confirm":     stateFromIndex(c.depositState.Load()),
		"market-confirm":      stateFromIndex(c.marketConfirmState.Load()),
		"market-projector":    stateFromIndex(c.projectorState.Load()),
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
