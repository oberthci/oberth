package model

import (
	"strings"
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

// #1518: an eviction after a continue reads as such inside the assisted
// line; one while parked, with nobody acting, is not assisted.
func TestRunAssistReasonNamesEviction(t *testing.T) {
	held := time.Date(2026, 11, 10, 5, 6, 39, 0, time.UTC)
	message := "Pod ephemeral local storage usage exceeds the total limit of containers 1216Mi."
	eviction := AssistAbandon{Pod: "wf-ts-1", Step: "test-service", Reason: AssistAbandonEvicted, HeldSince: held, AbandonedAt: held.Add(20 * time.Minute), Eviction: message}.Event()
	cont := RunAssistEvent{At: held.Add(3 * time.Minute), Actor: "admin@host", Step: "test-service", Pod: "wf-ts-1", Kind: RunAssistContinue}
	record := RunAssistRecord{Events: []RunAssistEvent{cont, eviction}}
	want := "assisted: step test-service continued without changes by admin@host; step test-service evicted after continue: ephemeral-storage (" + message + "); not evidence"
	if got := record.Reason(); got != want {
		t.Fatalf("reason = %q, want %q", got, want)
	}
	if !record.SweepAbandoned("test-service") || !record.PodActivities()["wf-ts-1"].Evicted {
		t.Fatalf("eviction not attributed: %+v", record.PodActivities())
	}
	parked := RunAssistRecord{Events: []RunAssistEvent{eviction}}
	if got := parked.Reason(); got != "step test-service evicted while held: ephemeral-storage ("+message+")" {
		t.Fatalf("parked eviction reason = %q", got)
	}
	long := AssistAbandon{Step: "s", Reason: AssistAbandonEvicted, Eviction: strings.Repeat("x", 500)}.Event()
	if len([]rune(long.Eviction)) != MaximumAssistEvictionLength {
		t.Fatalf("eviction message not bounded: %d", len(long.Eviction))
	}
}
