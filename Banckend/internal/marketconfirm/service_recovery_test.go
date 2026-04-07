package marketconfirm

import (
	"context"
	"testing"
	"time"
)

func TestRunRecoveryTaskRetriesUntilTerminal(t *testing.T) {
	svc := &Service{
		rootCtx:    context.Background(),
		retryBase:  time.Millisecond,
		retryLimit: 2 * time.Millisecond,
	}
	attempts := 0
	svc.runRecoveryTask("sig-1", Submission{Signature: "sig-1"}, func(Submission) (bool, bool) {
		attempts++
		if attempts < 3 {
			return true, false
		}
		return false, true
	})
	if attempts != 3 {
		t.Fatalf("expected 3 attempts, got %d", attempts)
	}
}

func TestRunRecoveryTaskStopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	svc := &Service{
		rootCtx:    ctx,
		retryBase:  100 * time.Millisecond,
		retryLimit: 100 * time.Millisecond,
	}
	attempts := 0
	svc.runRecoveryTask("sig-2", Submission{Signature: "sig-2"}, func(Submission) (bool, bool) {
		attempts++
		cancel()
		return true, false
	})
	if attempts != 1 {
		t.Fatalf("expected 1 attempt after cancel, got %d", attempts)
	}
}
