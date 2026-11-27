package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestFailedHistoryReleasesScratchBeforeNextBuilder(t *testing.T) {
	for _, mode := range []string{"failure", "crash", "cancel", "deadline", "cleanup-failure"} {
		t.Run(mode, func(t *testing.T) { checkFailedHistory(t, mode) })
	}
}

func TestInterruptedHistoryScratchIsRemovedBeforeStartup(t *testing.T) {
	f := newExportWaitFixture(t)
	if err := f.cache.Close(); err != nil {
		t.Fatal(err)
	}
	// Recreate the on-disk state left when the server dies during index-pack.
	dir := filepath.Join(f.cache.config.Scratch, "oberth-export-interrupted")
	packdir := filepath.Join(dir, "git", "objects", "pack")
	if err := os.MkdirAll(packdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(packdir, "source.pack"), []byte("interrupted pack"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(packdir, 0700) })
	if err := os.Chmod(packdir, 0500); err != nil {
		t.Fatal(err)
	}
	if c, err := New(t.Context(), f.cache.config); err == nil {
		_ = c.Close()
		t.Fatal("startup accepted unremoved history scratch")
	}
	if err := os.Chmod(packdir, 0700); err != nil {
		t.Fatal(err)
	}
	c, err := New(t.Context(), f.cache.config)
	if err != nil {
		t.Fatal("startup did not recover after scratch cleanup", err)
	}
	defer func() { _ = c.Close() }()
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("startup retained interrupted history scratch", err)
	}
	s, err := c.Stage(t.Context(), f.binding)
	if err != nil {
		t.Fatal("startup did not restore export capacity", err)
	}
	_ = s.Body.Close()
}

func checkFailedHistory(t *testing.T, mode string) {
	f := newExportWaitFixture(t)
	c := f.cache
	c.config.Permits = &sourcedelivery.Permits{}
	control := t.TempDir()
	job := installHistoryFault(t, c, control, mode)
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(context.Canceled)
	first := make(chan error, 1)
	go func() {
		s, err := c.Stage(ctx, f.binding)
		if err == nil {
			_ = s.Body.Close()
		}
		first <- err
	}()
	var gitdir string
	waitHistoryState(t, func() bool {
		data, err := os.ReadFile(filepath.Join(control, "ready"))
		gitdir = string(data)
		return err == nil && gitdir != ""
	})
	packdir := filepath.Join(gitdir, "objects", "pack")
	t.Cleanup(func() { _ = os.Chmod(packdir, 0700) })
	next, stopNext := context.WithCancel(t.Context())
	defer stopNext()
	second := make(chan exportWaitResult, 1)
	b := f.binding
	b.Template = "next-builder"
	go func() { s, err := c.Stage(next, b); second <- exportWaitResult{s, err} }()
	waitHistoryExport(t, c, b)
	releaseHistoryFault(t, mode, cancel, control)
	assertHistoryFault(t, mode, <-first)
	<-job
	if mode == "cleanup-failure" {
		stopNext()
		checkFailedHistoryDebt(t, f, packdir, second)
		go func() { s, err := c.Stage(t.Context(), b); second <- exportWaitResult{s, err} }()
	}
	r := <-second
	if r.err != nil {
		t.Fatal("next builder did not recover", r.err)
	}
	_ = r.stage.Body.Close()
	c.staged.workers.Wait()
	assertHistoryPermits(t, c, 0, 1)
}

func installHistoryFault(t *testing.T, c *Cache, control, mode string) <-chan sourcedelivery.Summary {
	t.Helper()
	real := c.config.Binary
	wrapper := filepath.Join(control, "history-fault")
	program := fmt.Sprintf(`#!/usr/bin/python3
import os,sys,time,signal
control=%q
if 'index-pack' in sys.argv and not os.path.exists(control+'/ready'):
 with open(control+'/ready','w') as f: f.write(os.getcwd())
 while not os.path.exists(control+'/release'): time.sleep(.001)
 if %q=='cleanup-failure': os.chmod('objects/pack',0o500)
 if %q=='crash': os.kill(os.getpid(),signal.SIGKILL)
 sys.exit(1)
os.execv(%q,[%q]+sys.argv[1:])
`, control, mode, mode, real, real)
	if err := os.WriteFile(wrapper, []byte(program), 0700); err != nil {
		t.Fatal(err)
	}
	c.config.Binary = wrapper
	job := make(chan sourcedelivery.Summary, 2)
	// Observe runs after buildArchive and before staged.complete's cleanup.
	c.config.Observe = func(s sourcedelivery.Summary) {
		data, readErr := os.ReadFile(filepath.Join(control, "ready"))
		if readErr != nil {
			t.Error(readErr)
		}
		_, err := os.Stat(filepath.Join(string(data), "objects", "pack", "source.pack"))
		if mode == "cleanup-failure" && err == nil {
			if c.config.Permits.Active[sourcedelivery.Export].Load() != 1 || len(c.export) != 1 {
				t.Error("failed scratch lost its export permit")
			}
		} else if err == nil {
			t.Error("failed history scratch outlives buildArchive")
		}
		job <- s
	}
	return job
}

func waitHistoryExport(t *testing.T, c *Cache, b sourcecontract.Binding) {
	t.Helper()
	waitHistoryState(t, func() bool {
		c.staged.mu.Lock()
		defer c.staged.mu.Unlock()
		e := c.staged.entries[archiveIdentity(b)]
		if e == nil || e.observation == nil {
			return false
		}
		s := e.observation.Finish("", nil)
		return s.Blocked && s.Phase == sourcedelivery.Export
	})
}

func releaseHistoryFault(t *testing.T, mode string, cancel context.CancelCauseFunc, control string) {
	t.Helper()
	switch mode {
	case "cancel":
		cancel(context.Canceled)
	case "deadline":
		cancel(context.DeadlineExceeded)
	default:
		if err := os.WriteFile(filepath.Join(control, "release"), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertHistoryFault(t *testing.T, mode string, err error) {
	t.Helper()
	if err == nil || mode == "cancel" && !errors.Is(err, context.Canceled) || mode == "deadline" && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("index-pack fault lost its load outcome", err)
	}
}

func checkFailedHistoryDebt(t *testing.T, f exportWaitFixture, packdir string, second <-chan exportWaitResult) {
	t.Helper()
	c := f.cache
	waitHistoryState(t, func() bool {
		c.staged.mu.Lock()
		defer c.staged.mu.Unlock()
		e := c.staged.entries[archiveIdentity(f.binding)]
		return e != nil && !e.building
	})
	c.staged.mu.Lock()
	failed := c.staged.entries[archiveIdentity(f.binding)]
	cleanupReported := errors.Is(failed.err, os.ErrPermission)
	c.staged.mu.Unlock()
	r := <-second
	if r.stage.Body != nil {
		_ = r.stage.Body.Close()
	}
	c.staged.workers.Wait()
	if !errors.Is(r.err, context.Canceled) || !cleanupReported {
		t.Fatal("cleanup debt lost its error or capacity ownership", r.err)
	}
	assertHistoryPermits(t, c, 1, 1)
	if err := c.staged.prune(nil); !errors.Is(err, os.ErrPermission) || len(c.export) != 1 {
		t.Fatal("failed pruning released cleanup debt", err)
	}
	if err := os.Chmod(packdir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := c.staged.prune(nil); err != nil {
		t.Fatal(err)
	}
}

func assertHistoryPermits(t *testing.T, c *Cache, exports, archives int64) {
	t.Helper()
	if int64(len(c.export)) != exports || c.config.Permits.Active[sourcedelivery.Export].Load() != exports || c.config.Permits.Active[sourcedelivery.Archive].Load() != archives {
		t.Fatal("cleanup debt lost its error or capacity ownership")
	}
}

func waitHistoryState(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for !ready() {
		select {
		case <-deadline.C:
			t.Fatal("history fixture did not reach its barrier")
		case <-tick.C:
		}
	}
}
