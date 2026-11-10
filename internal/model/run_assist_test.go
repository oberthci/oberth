package model

import (
	"testing"
	"time"
)

// A record with only sweep abandons belongs to a run nobody assisted: the
// attempt-0 failure is genuine, so the line carries neither "assisted:" nor
// "not evidence". Once an agent acted, the same sweep clause joins the
// assisted line.
func TestRunAssistReasonSweepOnlyIsNotAssisted(t *testing.T) {
	exit := 1
	held := time.Date(2026, 11, 9, 20, 14, 24, 0, time.UTC)
	sweep := AssistAbandon{Pod: "wf-test-1", Step: "test", Reason: AssistAbandonTTL, HeldSince: held, AbandonedAt: held.Add(20 * time.Minute), FailedExitCode: &exit}.Event()
	record := RunAssistRecord{Events: []RunAssistEvent{sweep}}
	if got, want := record.Reason(), "step test failed at attempt 0 (exit 1); hold abandoned unserviced after 20m0s (ttl)"; got != want {
		t.Fatalf("sweep-only reason = %q, want %q", got, want)
	}
	exec := RunAssistEvent{At: held.Add(time.Minute), Actor: "uplink-a", Step: "test", Pod: "wf-test-1", Kind: RunAssistExec}
	record = RunAssistRecord{Events: []RunAssistEvent{exec, sweep}}
	if got, want := record.Reason(), "assisted: step test inspected by uplink-a; step test failed at attempt 0 (exit 1); hold abandoned after 20m0s (ttl); not evidence"; got != want {
		t.Fatalf("worked reason = %q, want %q", got, want)
	}
	if got := (RunAssistRecord{}).Reason(); got != "" {
		t.Fatalf("empty record reason = %q", got)
	}
}
