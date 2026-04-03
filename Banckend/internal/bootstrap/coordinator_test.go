package bootstrap

import "testing"

func TestCoordinatorDefaultsToInitAndNotReady(t *testing.T) {
	c := NewCoordinator(nil, nil, nil, nil, nil, 0)

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
	if status["gateway_write_ready"] != false {
		t.Fatalf("expected write ready false, got %v", status["gateway_write_ready"])
	}
}
