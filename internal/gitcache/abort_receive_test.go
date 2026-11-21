package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestAbortedPassedReceiveForgetsExactlyItsAdmission(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"ref transaction failure", "cancelled", "delivered"} {
		t.Run(mode, func(t *testing.T) { checkAbortedReceive(t, mode) })
	}
}
func checkAbortedReceive(t *testing.T, mode string) {
	t.Helper()
	f := gittest.New(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	stash := map[string]bool{"other-receive": true}
	var passed string
	aborts, delivered := 0, 0
	c := configuredReceiveCache(t, f, func(cfg *gitcache.Config) {
		cfg.PreReceive = func(_ context.Context, e gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
			passed = e.ID
			stash[e.ID] = true
			if mode == "cancelled" {
				cancel()
			}
			if mode == "ref transaction failure" {
				return os.WriteFile(filepath.Join(f.Store(), "refs", "heads", "feature.lock"), nil, 0600)
			}
			return nil
		}
		cfg.AbortReceive = func(id string) {
			aborts++
			if id != passed {
				t.Error("forgot another receive", id)
			}
			delete(stash, id)
		}
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	r.Deliver = func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		delivered++
		delete(stash, e.ID)
		return nil, nil
	}
	var output bytes.Buffer
	r.Output = &output
	err := c.Receive(ctx, r)
	checkAbortOutcome(
		t,
		mode,
		abortOutcome{
			passed:    passed,
			aborts:    aborts,
			delivered: delivered,
			stash:     stash,
			err:       err,
			wire:      output.String(),
		},
	)
}

type abortOutcome struct {
	passed            string
	aborts, delivered int
	stash             map[string]bool
	err               error
	wire              string
}

func checkAbortOutcome(t *testing.T, mode string, o abortOutcome) {
	t.Helper()
	if mode == "delivered" {
		if o.err != nil || o.aborts != 0 || o.delivered != 1 {
			t.Fatalf("delivery %v aborts=%d calls=%d", o.err, o.aborts, o.delivered)
		}
	} else {
		checkFailedAbort(t, mode, o)
	}
	if !o.stash["other-receive"] || o.stash[o.passed] {
		t.Fatal("stash not scoped", o.stash)
	}
}

func checkFailedAbort(t *testing.T, mode string, o abortOutcome) {
	t.Helper()

	if o.passed == "" || o.aborts != 1 || o.delivered != 0 {
		t.Fatalf("abort id=%s calls=%d delivered=%d error=%v", o.passed, o.aborts, o.delivered, o.err)
	}
	if mode == "cancelled" && !errors.Is(o.err, context.Canceled) {
		t.Fatal(o.err)
	}
	if mode == "ref transaction failure" && !strings.Contains(o.wire, "ng refs/heads/feature") {
		t.Fatal(o.wire)
	}
}
