package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestNativeDeliveryPrintsSavedTerminalLinesInOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		lines []string
	}{
		{"started", []string{"run r1 started", "  watch: https://ci.example/prefix/runs/r1"}},
		{"parked", []string{"feature parked; no run started. To run it: git push -o oberth.run=1"}},
		{"saved start failure", []string{"run r1 failed to start", "  fix: rerun r1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := gittest.New(t)
			c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil })
			r := gateReceive(f)
			r.AlwaysRun = tc.name != "parked"
			r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
			calls := 0
			r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { calls++; return tc.lines, nil }
			var diagnostics bytes.Buffer
			r.Diagnostics = &diagnostics
			if err := c.Receive(t.Context(), r); err != nil {
				t.Fatal(err)
			}
			if got, want := diagnostics.String(), strings.Join(tc.lines, "\n")+"\n"; got != want {
				t.Fatalf("pusher got %q want %q", got, want)
			}
			if calls != 1 {
				t.Fatal(calls)
			}
			if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
				t.Fatal("saved outcome requeued", err)
			}
		})
	}
}

func TestTemporarySaveDoesNotPrintOrAcknowledgeDeliveryLines(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		return []string{"not saved; no watch link"}, io.ErrClosedPipe
	}
	var diagnostics bytes.Buffer
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if diagnostics.Len() != 0 {
		t.Fatal(diagnostics.String())
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); err != nil {
		t.Fatal("save outage lost reservation", err)
	}
	if err := c.ReplayPending(t.Context(), func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		return []string{"watch: https://ci.example/runs/r1"}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if diagnostics.Len() != 0 {
		t.Fatal("disconnected replay printed presentation")
	}
}

func TestSavedFeedbackStillPrintsWhenRetentionCleanupFails(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	r := gateReceive(f)
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	line := "watch: https://ci.example/runs/saved"
	lock := ""
	r.Deliver = func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		lock = filepath.Join(f.Store(), "refs", "oberth", "keep", "receive-"+event.ID, f.Commit+".lock")
		if err := os.WriteFile(lock, []byte("cleanup failure"), 0600); err != nil {
			t.Error(err)
		}
		return []string{line}, nil
	}
	var diagnostics bytes.Buffer
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("cleanup fault did not fire")
	}
	if diagnostics.String() != line+"\n" {
		t.Fatal("saved feedback lost", diagnostics.String())
	}
	if err := os.Remove(lock); err != nil {
		t.Fatal(err)
	}
}
