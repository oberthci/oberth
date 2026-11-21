package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestNativeGatePreservesPermanentFindingVersusUnavailable(t *testing.T) {
	t.Parallel()
	for _, code := range []publicerr.Code{publicerr.Forbidden, publicerr.Unavailable} {
		t.Run(string(code), func(t *testing.T) {
			checkGateClassification(t, code)
		})
	}
}

func TestAdmissionCallbackRefusalKeepsConfiguredDeadline(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	timers := 0
	c := receiveCache(t, f, func(ctx context.Context, _ gitcache.ReceiveEvent, _ *gitcache.ReceiveSource) error {
		if _, timed := ctx.Deadline(); !timed {
			return errors.New("admission duration was not applied")
		}
		timers++
		return publicerr.New(publicerr.Unavailable, "admission timed out; push again",
			"Try this push again.", context.DeadlineExceeded)
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, f, gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit})
	var diagnostics bytes.Buffer
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); !errors.Is(err, context.DeadlineExceeded) || publicerr.Permanent(err) {
		t.Fatal("caller timeout not unavailable", err)
	}
	if timers != 1 || !strings.Contains(diagnostics.String(), "admission timed out; push again") {
		t.Fatalf("timers=%d diagnostic=%s", timers, &diagnostics)
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/feature"); err == nil {
		t.Fatal("late approval moved ref")
	}
}

func checkGateClassification(t *testing.T, code publicerr.Code) {
	t.Helper()
	f := gittest.New(t)
	refusal := publicerr.New(code, "admission dependency is unavailable", "Try this push again.", nil)
	refusal.Detail = "  pipeline.field\n    fix: preserve this exact command"
	fail := true
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		if fail {
			return refusal
		}
		return nil
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	change := gitcache.RefUpdate{Ref: "refs/heads/feature", NewSHA: f.Commit}
	r.Input = receiveRefInput(t, f, change)
	var diagnostics bytes.Buffer
	r.Diagnostics = &diagnostics
	err := c.Receive(t.Context(), r)
	if !errors.Is(err, refusal) || publicerr.Classify(err).Code() != code {
		t.Fatalf("classification lost: %v", err)
	}
	if publicerr.Permanent(err) != (code == publicerr.Forbidden) {
		t.Fatal("permanence lost", err)
	}
	if strings.TrimSuffix(diagnostics.String(), "\n") != refusal.Error() {
		t.Fatal(diagnostics.String())
	}
	if _, err = c.ResolveRef(t.Context(), gittest.Repository, change.Ref); err == nil {
		t.Fatal("ref moved on admission error")
	}
	if _, err = c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("stale gate failure", err)
	}
	fail = false
	r.Input = receiveRefInput(t, f, change)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err = c.Receive(t.Context(), r); err != nil {
		t.Fatal("retry blocked", err)
	}
}
