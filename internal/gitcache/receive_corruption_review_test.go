package gitcache

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCorruptReceiveOutboxIsolatesRepositoryAndPreservesEvidence(t *testing.T) {
	repository := newTestRepository(t)
	cache, err := newBackendTestCache(t, Config{Root: filepath.Join(t.TempDir(), "cache"), Upstream: func(string) (string, error) { return repository.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"healthy", "broken"} {
		if _, err = cache.Ensure(t.Context(), name); err != nil {
			t.Fatal(err)
		}
	}
	good := receiveReservation{Version: receiveReservationVersion, ID: "healthy-event", Repo: "healthy", Actor: "tester", State: receiveReady, Before: map[string]string{}, Updates: []RefUpdate{{Ref: "refs/heads/main", NewSHA: repository.initialSHA}}}
	if err = cache.writeReservation(good); err != nil {
		t.Fatal(err)
	}
	corrupt := []byte(`{"version":1,"actor":"original evidence",`)
	path, err := cache.reservationPath("broken")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, corrupt, 0600); err != nil {
		t.Fatal(err)
	}
	delivered := 0
	handler := ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		if event.Repo != "healthy" {
			t.Fatalf("fabricated event: %+v", event)
		}
		delivered++
		return nil
	})
	if err = cache.ReplayPending(t.Context(), handler); err != nil {
		t.Fatal("one corrupt repository stopped startup", err)
	}
	if delivered != 1 {
		t.Fatalf("healthy replay blocked: %d", delivered)
	}
	pending, err := cache.PendingReceives()
	if err != nil || len(pending) != 1 || pending[0].Repo != "broken" || pending[0].State != "quarantined" {
		t.Fatalf("quarantine not visible: %+v %v", pending, err)
	}
	for name, action := range map[string]func() error{
		"refresh":       func() error { _, err := cache.Ensure(t.Context(), "broken"); return err },
		"fetch refresh": func() error { _, err := cache.EnsureForFetch(t.Context(), "broken"); return err },
		"publish":       func() error { return cache.SyncBranch(t.Context(), "broken", "main", repository.initialSHA, true) },
		"receive": func() error {
			return cache.Receive(t.Context(), "broken", "tester", "", bytes.NewReader(nil), io.Discard, io.Discard, handler)
		},
	} {
		if err := action(); err == nil {
			t.Fatalf("%s mutated quarantined repository", name)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(raw, corrupt) {
		t.Fatal("corrupt evidence changed", err)
	}
	if _, err = cache.Ensure(t.Context(), "healthy"); err != nil {
		t.Fatal("healthy repository blocked", err)
	}
	// A verified operator repair can be replayed without restarting the process.
	repaired := good
	repaired.Repo = "broken"
	repaired.ID = "repaired"
	repaired.Updates = nil
	if err = cache.writeReservation(repaired); err != nil {
		t.Fatal(err)
	}
	if err = cache.ReplayPending(t.Context(), handler); err != nil {
		t.Fatal(err)
	}
	if _, err = cache.Ensure(t.Context(), "broken"); err != nil {
		t.Fatal("repaired repository remained blocked", err)
	}
}
