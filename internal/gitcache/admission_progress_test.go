package gitcache

import (
	"context"
	"k8s.io/utils/clock"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type admissionIdleTimer struct{ stops, resets int }

func (t *admissionIdleTimer) Stop() bool               { t.stops++; return true }
func (t *admissionIdleTimer) Reset(time.Duration) bool { t.resets++; return true }

func TestAdmissionAndHookHandoffsExcludeIdleExpiry(t *testing.T) {
	c, session, quarantine := gateFixture(t)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	progress := &commandProgress{now: (clock.RealClock{}).Now, timer: &admissionIdleTimer{}, idle: time.Hour, cancel: cancel}
	defer progress.close()
	active := true
	session.progress = &receiveProgress{progress: progress, end: func() { active = false }}
	c.config.PreReceive = func(context.Context, ReceiveEvent, *ReceiveSource) error {
		progress.expire()
		if active {
			t.Error("admission counted as client inactivity")
		}
		return context.Cause(ctx)
	}
	held := 0
	if err := c.gateOperation(ctx, session, gateRequest{Quarantine: quarantine}, &held); err != nil {
		t.Fatal(err)
	}
	// The hook must still save ready, exit, and let Git start its next hook.
	progress.expire()
	if ctx.Err() != nil || active {
		t.Fatal("hook handoff spent transport idle", context.Cause(ctx))
	}
	if err := c.gateOperation(ctx, session, gateRequest{Phase: "preparing"}, &held); err != nil {
		t.Fatal(err)
	}
	progress.expire()
	if ctx.Err() != nil || active {
		t.Fatal("native preparation spent transport idle", context.Cause(ctx))
	}
}

func TestNativeCommandReusesProvidedProgress(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	repo := filepath.Join(t.TempDir(), "repository")
	if out, err := exec.CommandContext(t.Context(), "/usr/bin/git", "init", "--bare", repo).CombinedOutput(); err != nil {
		t.Fatalf("initialize repository: %v: %s", err, out)
	}
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	timer := &admissionIdleTimer{}
	progress := &commandProgress{now: (clock.RealClock{}).Now, timer: timer, idle: time.Hour, cancel: cancel}
	if err = c.run(ctx, command{dir: repo, args: []string{"upload-pack", "--advertise-refs", "."}, progress: progress}); err != nil {
		t.Fatal(err)
	}
	if timer.resets == 0 || !progress.closed {
		t.Fatal("native I/O bypassed its supplied progress timer", timer)
	}
}

func TestPreparationExcludesIdleExpiryWhileWaitingForGate(t *testing.T) {
	c, session, _ := gateFixture(t)
	record, err := readReservation(session.store)
	if err != nil {
		t.Fatal(err)
	}
	record.State = "ready"
	if err = writeReservation(session.store, record); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	timer := &admissionIdleTimer{}
	progress := &commandProgress{now: (clock.RealClock{}).Now, timer: timer, idle: time.Hour, cancel: cancel}
	defer progress.close()
	waiting := make(chan struct{})
	session.progress = &receiveProgress{progress: progress, end: func() { close(waiting) }}
	if err = c.lock(ctx, session.store); err != nil {
		t.Fatal(err)
	}
	held := 0
	done := make(chan error, 1)
	go func() { done <- c.gateOperation(ctx, session, gateRequest{Phase: "prepared"}, &held) }()
	<-waiting
	for range 32 {
		progress.expire()
	}
	waitingCause := context.Cause(ctx)
	c.unlock(session.store)
	err = <-done
	if held > 0 {
		c.unlock(session.store)
	}
	if err != nil || waitingCause != nil || held != 1 {
		t.Fatal("repository gate wait spent transport idle", err, waitingCause, held)
	}
	if [3]int{timer.stops, timer.resets, progress.paused} != [3]int{1, 0, 1} {
		t.Fatal("preparation restarted transport before a response write", timer, progress.paused)
	}
	progress.expire()
	if context.Cause(ctx) != nil {
		t.Fatal("reference publication spent transport idle")
	}
}
