package bootstrap

import "testing"

func TestCoordinatorDefaultsToInitAndNotReady(t *testing.T) {
	c := NewCoordinator(nil, nil, nil, nil, nil, nil, nil, nil, 0)

	if c.OrdersReady() {
		t.Fatal("expected write gate to be disabled before start")
	}

	status := c.Status()
	if status["writer"] != StateInit {
		t.Fatalf("expected writer init, got %v", status["writer"])
	}
	if status["funds"] != StateInit {
		t.Fatalf("expected funds init, got %v", status["funds"])
	}
	if status["matcher"] != StateInit {
		t.Fatalf("expected matcher init, got %v", status["matcher"])
	}
	if status["pusher"] != StateInit {
		t.Fatalf("expected pusher init, got %v", status["pusher"])
	}
	if status["deposit-confirm"] != StateInit {
		t.Fatalf("expected deposit-confirm init, got %v", status["deposit-confirm"])
	}
	if status["market-confirm"] != StateInit {
		t.Fatalf("expected market-confirm init, got %v", status["market-confirm"])
	}
	if status["market-projector"] != StateInit {
		t.Fatalf("expected market-projector init, got %v", status["market-projector"])
	}
	if status["settlement"] != StateInit {
		t.Fatalf("expected settlement init, got %v", status["settlement"])
	}
	if status["gateway_write_ready"] != false {
		t.Fatalf("expected write ready false, got %v", status["gateway_write_ready"])
	}
}

func TestGateDefaultsIncludeConfirmModules(t *testing.T) {
	g := NewGate()
	status := g.Status()

	if status["deposit-confirm"] != StateInit {
		t.Fatalf("expected deposit-confirm init, got %v", status["deposit-confirm"])
	}
	if status["market-confirm"] != StateInit {
		t.Fatalf("expected market-confirm init, got %v", status["market-confirm"])
	}
	if status["market-projector"] != StateInit {
		t.Fatalf("expected market-projector init, got %v", status["market-projector"])
	}
}
