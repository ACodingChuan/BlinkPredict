package logging

import (
	"testing"
	"time"
)

func TestFormatValueNilTimePointer(t *testing.T) {
	var ts *time.Time

	if got := formatValue(ts); got != nil {
		t.Fatalf("expected nil for nil *time.Time, got %v", got)
	}
}

func TestFormatValueTimeUsesRFC3339NanoUTC(t *testing.T) {
	ts := time.Date(2026, 4, 13, 9, 7, 8, 123456789, time.FixedZone("UTC+8", 8*3600))

	got := formatValue(ts)
	want := "2026-04-13T01:07:08.123456789Z"
	if got != want {
		t.Fatalf("unexpected formatted time: got=%v want=%s", got, want)
	}
}
