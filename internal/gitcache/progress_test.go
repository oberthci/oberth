package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestNativeIdleAndTotalBudgetsStopAndJoinStalledGit(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	for _, mode := range []string{"idle", "local-total", "transfer-total"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			fifo := filepath.Join(dir, "wait")
			if err := syscall.Mkfifo(fifo, 0600); err != nil {
				t.Fatal(err)
			}
			binary := filepath.Join(dir, "git")
			if err := os.WriteFile(binary, []byte("#!/bin/sh\nread line < '"+fifo+"'\n"), 0700); err != nil {
				t.Fatal(err)
			}
			c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: binary})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = c.Close() }()
			c.budgets = gitBudgets{idle: time.Hour, local: time.Hour, transfer: time.Hour}
			args := []string{"status"}
			switch mode {
			case "idle":
				c.budgets.idle = time.Millisecond
				args = []string{"fetch"}
			case "local-total":
				c.budgets.local = time.Millisecond
			case "transfer-total":
				c.budgets.transfer = time.Millisecond
				args = []string{"fetch"}
			}
			err = c.run(t.Context(), command{dir: dir, args: args})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("budget ignored", err)
			}
		})
	}
	budgets := defaultGitBudgets()
	if budgets.local != 10*time.Minute || budgets.transfer != time.Hour || budgets.idle != time.Minute {
		t.Fatal(budgets)
	}
}
func TestProgressCountsOnlyPositiveBytes(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)
	p := watchProgress(cancel, time.Hour)
	defer p.close()
	reader := commandReader{Reader: strings.NewReader("data"), progress: p}
	writer := commandWriter{Writer: io.Discard, progress: p}
	if _, err := io.Copy(writer, reader); err != nil {
		t.Fatal(err)
	}
	p.touch(0)
	if ctx.Err() != nil {
		t.Fatal("progress cancelled command")
	}
}
func TestExportAndGCBudgetsCancelWork(t *testing.T) {
	// Keep ownership release/reopen isolated from other tests' subprocess startup.
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: "/usr/bin/git"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	c.budgets.local = time.Millisecond
	c.config.LiveOwners = func(ctx context.Context, _ string) (OwnerPage, error) {
		<-ctx.Done()
		return OwnerPage{}, context.Cause(ctx)
	}
	if err = c.GC(t.Context()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("GC ignored budget", err)
	}
}
