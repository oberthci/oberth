package gitcache

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceiveRepairUsesOnlyCompleteValidPredecessors(t *testing.T) {
	oid := strings.Repeat("a", 40)
	for _, row := range []struct {
		data  string
		known bool
	}{
		{`{"before":{"refs/heads/topic":"` + oid + `"},"event":`, true},
		{`{"before":{"refs/heads/topic":"` + oid, false},
		{`{"before":{"refs/heads/topic":"not-an-object"},"event":`, false},
		{`{"before":{"refs/oberth/keep/foreign":"` + oid + `"},"event":`, false},
	} {
		record := receiveRepairFacts([]byte(row.data))
		if (record.Before != nil) != row.known {
			t.Errorf("unexpected predecessor evidence: known=%t, want=%t", record.Before != nil, row.known)
		}
	}
}

func TestCorruptReceiveIdentityNamesExactBytes(t *testing.T) {
	store := t.TempDir()
	previous := ""
	for _, raw := range []string{`{"before":`, `{"before": `} {
		if err := os.WriteFile(filepath.Join(store, "oberth-receive.json"), []byte(raw), 0600); err != nil {
			t.Fatal(err)
		}
		record, owner, err := readReceiveRepair(store, "origin/team/widget")
		if err != nil || record.State != "corrupt" || owner != "" || record.Event.ID == previous {
			t.Fatal("corrupt identity lost its exact snapshot or invented an owner", record, owner, err)
		}
		previous = record.Event.ID
	}
}

func TestCorruptAbandonWaitsForRepositoryGate(t *testing.T) {
	c := &Cache{config: Config{Root: t.TempDir()}, gates: &repositoryGates{repos: make(map[string]chan struct{})}}
	store, err := c.path("origin/team/widget")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(store, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(store, "oberth-receive.json")
	if err = os.WriteFile(path, []byte(`{"before":`), 0600); err != nil {
		t.Fatal(err)
	}
	record, _, err := readReceiveRepair(store, "origin/team/widget")
	if err != nil {
		t.Fatal(err)
	}
	c.repositoryGate(store) <- struct{}{}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if err = c.AbandonReceive(ctx, "origin/team/widget", record.Event.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("abandon escaped the repository gate", err)
	}
	c.unlock(store)
	if _, err = os.Stat(path); err != nil {
		t.Fatal("blocked abandon changed the record", err)
	}
}

func TestCorruptRepairDoesNotUseAnotherRepositoryEvidence(t *testing.T) {
	store := t.TempDir()
	r := Reservation{State: "ready", Before: map[string]string{"refs/heads/topic": strings.Repeat("a", 40)},
		Event: ReceiveEvent{ID: strings.Repeat("b", 32), Repository: "origin/team/other", Actor: "agent", Fingerprint: "key"}}
	if err := writeReservation(store, r); err != nil {
		t.Fatal(err)
	}
	got, owner, err := readReceiveRepair(store, "origin/team/widget")
	if err != nil || got.State != "corrupt" || got.Before != nil || owner != "" {
		t.Fatal("foreign reservation supplied rollback or retention authority", got, owner, err)
	}
}
