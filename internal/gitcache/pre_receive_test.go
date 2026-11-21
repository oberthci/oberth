package gitcache_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestPreReceiveQuarantineRefusesWholePushWithEveryFinding(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "file.txt", []byte("pushed quarantine\n"))
	next := f.CommitFile(t, "push")
	var c *gitcache.Cache
	var source *gitcache.ReceiveSource
	calls := 0
	findings := errors.New(
		"build.image: registry is not allowed\nfix: use the approved image\ndep" +
			"loy.secret: grant is missing\nfix: approve the exact grant",
	)
	c = receiveCache(t, f, func(ctx context.Context, event gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		calls++
		source = s
		if err := checkQuarantine(ctx, quarantineCheck{cache: c, source: s, commit: next}); err != nil {
			t.Error(err)
			return err
		}
		if len(event.Updates) != 2 {
			t.Error(event.Updates)
		}
		export, err := s.Checkout(
			ctx,
			gittest.Repository,
			sourcecontract.Ref{Name: "refs/heads/first", ObjectSHA: next, CommitSHA: next},
		)
		if err != nil || export.Ref.CommitSHA != next || export.DefaultBranch.CommitSHA != f.Commit {
			t.Errorf("checkout %+v %v", export, err)
		}
		return findings
	})
	var output, diagnostics bytes.Buffer
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(
		t,
		f,
		[]gitcache.RefUpdate{
			{Ref: "refs/heads/first", NewSHA: next},
			{Ref: "refs/heads/second", NewSHA: next},
		},
		nil,
	)
	r.Output = &output
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); !errors.Is(err, findings) {
		t.Fatalf("receive: %v", err)
	}
	if calls != 1 ||
		!strings.Contains(diagnostics.String(), findings.Error()) ||
		!strings.Contains(output.String(), "pre-receive hook declined") {
		t.Fatalf("calls=%d diagnostic=%s wire=%s", calls, &diagnostics, &output)
	}
	checkRejectedReceive(t, c, f, source)
}

type quarantineCheck struct {
	cache  *gitcache.Cache
	source *gitcache.ReceiveSource
	commit string
}

func checkQuarantine(ctx context.Context, q quarantineCheck) error {
	at := gitport.Revision{Repository: gittest.Repository, Commit: q.commit}
	if _, err := q.cache.ReadFile(ctx, at, "file.txt"); err == nil {
		return fmt.Errorf("pushed object was already stored")
	}
	file, err := q.source.ReadFile(ctx, at, "file.txt")
	if err != nil || string(file.Bytes) != "pushed quarantine\n" {
		return fmt.Errorf("quarantine %+v: %w", file, err)
	}
	return nil
}

func gateReceive(f *gittest.Fixture) gitcache.ReceiveRequest {
	return gitcache.ReceiveRequest{
		Repository:  gittest.Repository,
		Actor:       "agent",
		Fingerprint: "key",
		Admission:   gitcache.ReleaseAdmission{DefaultBranch: "refs/heads/main", SHA: f.Commit},
		Output:      io.Discard,
	}
}

func TestPreReceiveApprovalIsNotRepeatedByDeliveryReplay(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "file.txt", []byte("pushed quarantine\n"))
	next := f.CommitFile(t, "push")
	checks, delivered := 0, 0
	var original gitcache.ReceiveEvent
	var source *gitcache.ReceiveSource
	c := receiveCache(t, f, func(ctx context.Context, e gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		checks++
		original = e
		source = s
		return checkGateFile(ctx, s, next)
	})
	producer := "origin/team/producer"
	if err := c.Init(t.Context(), producer); err != nil {
		t.Fatal(err)
	}
	f.Git(t, filepath.Join(f.Root, "cache", "origin", "team", "producer.git"), "fetch", f.Work, "HEAD:refs/heads/main")
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: next}}
	r := gateReceive(f)
	r.Input = receiveBatchInput(t, f, updates, []string{"oberth.run=1"})
	r.Deliver = func(ctx context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		delivered++
		assertAttribution(t, e, original)
		if _, err := c.ReadFile(ctx, gitport.Revision{Repository: gittest.Repository, Commit: next}, "file.txt"); err != nil {
			t.Error(err)
		}
		if delivered == 1 {
			return nil, io.ErrClosedPipe
		}
		return nil, nil
	}
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	if err := c.ReplayPending(t.Context(), r.Deliver); err != nil {
		t.Fatal(err)
	}
	if checks != 1 || delivered != 2 {
		t.Fatalf("checks=%d delivery=%d", checks, delivered)
	}
	if _, err := source.ReadFile(
		t.Context(),
		gitport.Revision{Repository: producer, Commit: next},
		"file.txt",
	); err == nil {
		t.Fatal("reader survived admission")
	}
	if dirs, err := os.ReadDir(filepath.Join(f.Root, "scratch")); err != nil || len(dirs) != 0 {
		t.Fatalf("scratch retained: %v %v", dirs, err)
	}
}

func TestParkedReceiveBypassesAdmissionAndOperatorCannotPark(t *testing.T) {
	t.Parallel()
	for _, operator := range []bool{false, true} {
		t.Run(fmt.Sprint(operator), func(t *testing.T) {
			f := gittest.New(t)
			calls := 0
			c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
				calls++
				return io.ErrClosedPipe
			})
			updates := []gitcache.RefUpdate{{Ref: "refs/heads/parked", NewSHA: f.Commit}}
			r := gateReceive(f)
			r.AlwaysRun = operator
			r.Input = receiveBatchInput(t, f, updates, []string{"oberth.assist=1", "oberth.run=0"})
			r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
			err := c.Receive(t.Context(), r)
			if operator {
				if !errors.Is(err, io.ErrClosedPipe) || calls != 1 {
					t.Fatalf("operator skipped gate %d %v", calls, err)
				}
			} else if err != nil || calls != 0 {
				t.Fatalf("parked checked %d %v", calls, err)
			}
			_, err = c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/parked")
			if (err == nil) == operator {
				t.Fatalf("ref mutation: %v", err)
			}
		})
	}
}

func TestRequestedRunWithoutGateIsRefused(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: f.Commit}}
	r := gateReceive(f)
	r.Input = receiveBatchInput(t, f, updates, []string{"oberth.run=1"})
	var diagnostics bytes.Buffer
	r.Diagnostics = &diagnostics
	if err := c.Receive(t.Context(), r); err == nil {
		t.Fatal("unchecked run accepted")
	}
	if !strings.Contains(diagnostics.String(), "Try the same push again") {
		t.Fatal(diagnostics.String())
	}
	if _, err := c.ResolveRef(t.Context(), gittest.Repository, "refs/heads/feature"); err == nil {
		t.Fatal("unchecked ref moved")
	}
}

func TestRefusedPushCanRepeatSameCommitOnceApproved(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "next", []byte("next"))
	next := f.CommitFile(t, "push")
	allow := false
	checks, delivered := 0, 0
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		checks++
		if !allow {
			return io.ErrClosedPipe
		}
		return nil
	})
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: next}}
	for _, approved := range []bool{false, true} {
		allow = approved
		r := gateReceive(f)
		r.AlwaysRun = true
		r.Input = receiveBatchInput(t, f, updates, nil)
		r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { delivered++; return nil, nil }
		err := c.Receive(t.Context(), r)
		if (err == nil) != approved {
			t.Fatalf("approved=%v error=%v", approved, err)
		}
	}
	if checks != 2 || delivered != 1 {
		t.Fatalf("checks=%d delivered=%d", checks, delivered)
	}
}

func TestPreReceiveCheckoutIncludesQuarantinedRawTag(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	f.Write(t, "file.txt", []byte("pushed quarantine\n"))
	next := f.CommitFile(t, "push")
	f.Git(t, f.Work, "tag", "-a", "v1", "-m", "release", next)
	tag := f.Git(t, f.Work, "rev-parse", "refs/tags/v1")
	called := false
	c := receiveCache(t, f, func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		called = true
		export, err := s.Checkout(ctx, gittest.Repository, sourcecontract.Ref{Name: "refs/tags/v1", ObjectSHA: tag})
		if err != nil {
			return err
		}
		if export.Ref.CommitSHA != next ||
			export.Ref.ObjectSHA != tag ||
			len(export.Tags) != 1 ||
			export.Tags[0].ObjectSHA != tag {
			return errors.New("quarantined tag inventory changed")
		}
		return nil
	})
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: next}, {Ref: "refs/tags/v1", NewSHA: tag}}
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, updates, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("tag was not admitted")
	}
}

func TestPreReceivePausesTransportIdleDuringAdmission(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	var active atomic.Bool
	var begins, ends atomic.Int32
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		if active.Load() {
			return errors.New("admission counted as client inactivity")
		}
		return nil
	})
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: f.Commit}}
	r := gateReceive(f)
	// Isolate admission from response writes, which legitimately spend idle.
	r.Output = nil
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, updates, nil)
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	r.Begin = func() { active.Store(true); begins.Add(1) }
	r.End = func() { active.Store(false); ends.Add(1) }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if active.Load() || begins.Load() != 1 || ends.Load() != 2 {
		t.Fatalf("active=%v begin=%d end=%d", active.Load(), begins.Load(), ends.Load())
	}
}

func TestExplicitReceiveRetryPreservesAcceptedAttribution(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	c := receiveCache(t, f, nil)
	updates := []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: f.Commit}}
	r := gateReceive(f)
	r.Input = receiveBatchInput(t, f, updates, nil)
	var original gitcache.ReceiveEvent
	r.Deliver = func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		original = event
		return nil, io.ErrClosedPipe
	}
	if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	delivered := 0
	retry := func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		delivered++
		assertAttribution(t, event, original)
		return nil, nil
	}
	if err := c.RetryReceive(t.Context(), gittest.Repository, retry); err != nil {
		t.Fatal(err)
	}
	if delivered != 1 {
		t.Fatal(delivered)
	}
}

func checkRejectedReceive(t *testing.T, c *gitcache.Cache, f *gittest.Fixture, source *gitcache.ReceiveSource) {
	t.Helper()
	for _, ref := range []string{"refs/heads/first", "refs/heads/second"} {
		if _, err := c.ResolveRef(t.Context(), gittest.Repository, ref); err == nil {
			t.Fatal("ref moved", ref)
		}
	}
	if _, err := source.ReadFile(
		t.Context(),
		gitport.Revision{Repository: gittest.Repository, Commit: f.Commit},
		"file.txt",
	); err == nil {
		t.Fatal("quarantine reader escaped its call")
	}
	if _, err := c.PendingReceive(t.Context(), gittest.Repository); !os.IsNotExist(err) {
		t.Fatal("rejected reservation retained", err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/oberth/keep/"); refs != "" {
		t.Fatal("rejected receive kept roots", refs)
	}
}

func checkGateFile(ctx context.Context, s *gitcache.ReceiveSource, commit string) error {
	file, err := s.ReadFile(ctx, gitport.Revision{Repository: gittest.Repository, Commit: commit}, "file.txt")
	if err != nil {
		return err
	}
	if string(file.Bytes) != "pushed quarantine\n" {
		return errors.New("gate read the wrong source")
	}
	return nil
}

func TestTagRunsCannotBypassAdmission(t *testing.T) {
	for _, ref := range []string{"refs/tags/v1.2.3", "refs/tags/ordinary"} {
		t.Run(ref, func(t *testing.T) {
			f := gittest.New(t)
			calls := 0
			c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
				calls++
				return io.ErrClosedPipe
			})
			for _, options := range [][]string{nil, {"oberth.run=0"}} {
				r := gateReceive(f)
				r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: ref, NewSHA: f.Commit}}, options)
				r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
					t.Fatal("refused tag delivered")
					return nil, nil
				}
				if err := c.Receive(t.Context(), r); !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal(err)
				}
				if _, err := c.ResolveRef(t.Context(), gittest.Repository, ref); err == nil {
					t.Fatal("refused tag moved")
				}
			}
			if calls != 2 {
				t.Fatal(calls)
			}
		})
	}
}

func TestPreReceiveResponseWritesRemainIdleBounded(t *testing.T) {
	t.Parallel()
	f := gittest.New(t)
	var active, admitted, written atomic.Bool
	c := receiveCache(t, f, func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error {
		admitted.Store(true)
		return nil
	})
	r := gateReceive(f)
	r.AlwaysRun = true
	r.Input = receiveBatchInput(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/feature", NewSHA: f.Commit}}, nil)
	r.Output = receiveIdleWriter{active: &active, admitted: &admitted, written: &written}
	r.Begin = func() { active.Store(true) }
	r.End = func() { active.Store(false) }
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if !written.Load() || active.Load() {
		t.Fatal("response was not written inside its idle interval")
	}
}

type receiveIdleWriter struct{ active, admitted, written *atomic.Bool }

func (w receiveIdleWriter) Write(b []byte) (int, error) {
	if w.admitted.Load() {
		if !w.active.Load() {
			return 0, errors.New("response write escaped client idle bound")
		}
		w.written.Store(true)
	}
	return len(b), nil
}
