package funds

import (
	"encoding/json"
	"testing"

	"blinkpredict/banckend/internal/protocol"
)

func TestServiceApplyTerminalQueuesPendingForUnknownMatch(t *testing.T) {
	svc := &Service{
		manager:  NewManager(),
		inflight: NewInflightStore(),
	}

	raw := json.RawMessage(`{"match_event_id":"evt-late"}`)
	svc.applyTerminal("evt-late", PhaseConfirmed, raw)

	pt, ok := svc.inflight.TakePendingTerminal("evt-late")
	if !ok {
		t.Fatal("expected pending terminal to be stored")
	}
	if pt.Phase != PhaseConfirmed {
		t.Fatalf("expected confirmed phase, got %s", pt.Phase)
	}
	if string(pt.RawEvent) != string(raw) {
		t.Fatalf("expected raw event to be preserved, got %s", string(pt.RawEvent))
	}
}

func TestServiceApplyPendingLifecycleSubmittedAdvancesInflight(t *testing.T) {
	svc := &Service{
		manager:  NewManager(),
		inflight: NewInflightStore(),
	}
	svc.inflight.Register(&InflightMatch{
		MatchEventID: "evt-001",
		Phase:        PhasePendingApplied,
	})

	raw, err := json.Marshal(protocol.SettlementSubmittedEvent{
		MatchEventID: "evt-001",
		TxSignature:  "sig-abc",
	})
	if err != nil {
		t.Fatalf("marshal pending submitted event: %v", err)
	}

	svc.applyPendingLifecycle("evt-001", &PendingTerminal{
		MatchEventID: "evt-001",
		Phase:        PhaseSubmitted,
		RawEvent:     raw,
	})

	got, ok := svc.inflight.Get("evt-001")
	if !ok {
		t.Fatal("expected inflight entry to exist")
	}
	if got.Phase != PhaseSubmitted {
		t.Fatalf("expected phase submitted, got %s", got.Phase)
	}
	if got.SubmittedTxSignature != "sig-abc" {
		t.Fatalf("expected tx signature sig-abc, got %s", got.SubmittedTxSignature)
	}
}
