package bootstrap

import "sync"

type Gate struct {
	mu     sync.RWMutex
	status map[string]any
	ready  bool
}

func NewGate() *Gate {
	return &Gate{
		status: map[string]any{
			"writer":              StateInit,
			"funds":               StateInit,
			"matcher":             StateInit,
			"pusher":              StateInit,
			"deposit-confirm":     StateInit,
			"market-confirm":      StateInit,
			"market-projector":    StateInit,
			"settlement":          StateInit,
			"gateway_write_ready": false,
		},
	}
}

func (g *Gate) Set(module string, state ModuleState) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.status[module] = state
}

func (g *Gate) MarkReady() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ready = true
	g.status["gateway_write_ready"] = true
}

func (g *Gate) OrdersReady() bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.ready
}

func (g *Gate) Status() map[string]any {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make(map[string]any, len(g.status))
	for k, v := range g.status {
		out[k] = v
	}
	return out
}
