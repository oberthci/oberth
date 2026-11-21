package gitcache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

// slowForge makes every clone wait at a gate the test opens, the way a large
// first clone keeps transferring after its caller's deadline. It returns the
// release function and the file that counts started clones.
func slowForge(t *testing.T, f *gittest.Fixture, cfg *gitcache.Config) (func(), string) {
	t.Helper()
	dir := t.TempDir()
	gate, log, script := filepath.Join(dir, "gate"), filepath.Join(dir, "clones"), filepath.Join(dir, "git")
	if err := syscall.Mkfifo(gate, 0600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf("#!/bin/sh\nfor a do\n  if [ \"$a\" = clone ]; then\n    echo clone >> '%s'\n    read _ < '%s'\n    break\n  fi\ndone\nexec '%s' \"$@\"\n", log, gate, f.Binary)
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	cfg.Binary = script
	release := func() {
		w, err := os.OpenFile(gate, os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = w.WriteString("\n")
		if err = errors.Join(err, w.Close()); err != nil {
			t.Fatal(err)
		}
	}
	return release, log
}

func shortInit(t *testing.T, c *gitcache.Cache) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()
	return c.Init(ctx, gittest.Repository)
}

func TestFirstCloneOutlivesItsCallerAndAnnouncesItsRefs(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	release, log := slowForge(t, f, &cfg)
	announced := make(chan []gitcache.RefUpdate, 1)
	cfg.RefChanged = func(_ context.Context, repo string, updates []gitcache.RefUpdate) {
		if repo == gittest.Repository {
			announced <- updates
		}
	}
	c := reviewNew(t, cfg)
	for range 2 {
		err := shortInit(t, c)
		var unfinished *gitport.CloneUnfinishedError
		if !errors.As(err, &unfinished) || publicerr.Classify(err).Reason() != gitport.CloneUnfinishedReason || publicerr.Permanent(err) {
			t.Fatal("a clone past its caller's deadline was not reported as unfinished", err)
		}
		if strings.Contains(err.Error(), gitport.ForgeReason) {
			t.Fatal("an answering forge was reported unreachable", err)
		}
	}
	release()
	updates := <-announced
	if !slices.Contains(updates, gitcache.RefUpdate{Ref: "refs/heads/main", NewSHA: f.Commit}) {
		t.Fatal("the finished clone did not announce its refs", updates)
	}
	if err := c.Init(t.Context(), gittest.Repository); err != nil {
		t.Fatal(err)
	}
	started, err := os.ReadFile(log)
	if err != nil || strings.Count(string(started), "clone") != 1 {
		t.Fatal("callers did not share one first clone", string(started), err)
	}
}

func TestFirstCloneThatEndsInTimeAnnouncesNothing(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	announced := 0
	cfg.RefChanged = func(context.Context, string, []gitcache.RefUpdate) { announced++ }
	c := reviewNew(t, cfg)
	if err := c.Init(t.Context(), gittest.Repository); err != nil || announced != 0 {
		t.Fatal("a waited-for clone was announced or failed", announced, err)
	}
}

// A late clone announces a repository registered when it ends, under the ID
// it started with or registered only meanwhile (an upstream check, then repo
// add): an unregistered probe name, a repository removed during the clone and
// one registered again under another ID have no readers.
func TestLateCloneAnnouncesOnlyTheSameRegisteredRepository(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start, end int64
		announced  bool
	}{{"registered", 1, 1, true}, {"unregistered", 0, 0, false}, {"removed", 1, 0, false}, {"replaced", 1, 2, false}, {"registered during the clone", 0, 3, true}} {
		t.Run(tc.name, func(t *testing.T) {
			f := gittest.New(t)
			cfg := reviewConfig(t, f)
			release, _ := slowForge(t, f, &cfg)
			var mu sync.Mutex
			id := tc.start
			cfg.Upstream = func(context.Context, string) (gitcache.Remote, error) {
				mu.Lock()
				defer mu.Unlock()
				return gitcache.Remote{URL: f.Bare, DefaultBranch: "refs/heads/main", RepositoryID: id}, nil
			}
			announced := false
			cfg.RefChanged = func(context.Context, string, []gitcache.RefUpdate) { announced = true }
			c := reviewNew(t, cfg)
			if err := shortInit(t, c); err == nil {
				t.Fatal("a gated clone finished")
			}
			mu.Lock()
			id = tc.end
			mu.Unlock()
			release()
			gitcache.WaitFirstClones(c)
			if announced != tc.announced {
				t.Fatal("announcement differs from the registration", announced)
			}
		})
	}
}

// A first clone ends at its own bound even when its caller waits longer: the
// error is transient, nothing is published, and the next Init clones again.
func TestFirstCloneEndsAtItsBound(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	_, log := slowForge(t, f, &cfg)
	c := reviewNew(t, cfg)
	gitcache.SetFirstCloneBound(c, 300*time.Millisecond)
	for attempt := 1; attempt <= 2; attempt++ {
		err := c.Init(t.Context(), gittest.Repository)
		if err == nil || publicerr.Permanent(err) {
			t.Fatal("a clone past its bound did not end with a transient error", err)
		}
		if _, err = os.Stat(filepath.Join(cfg.Root, "origin", "team", "widget.git", "HEAD")); !os.IsNotExist(err) {
			t.Fatal("a clone ended by its bound was published", err)
		}
		started, err := os.ReadFile(log)
		if err != nil || strings.Count(string(started), "clone") != attempt {
			t.Fatal("the next Init did not start a new clone", attempt, string(started), err)
		}
	}
}

// Once Close has begun no first clone starts: an Init afterwards fails
// without cloning, and Inits racing Close leave no clone behind (run with
// -race, which also checks the wait against a late start).
func TestNoFirstCloneStartsOnceCloseBegins(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	_, log := slowForge(t, f, &cfg)
	var mu sync.Mutex
	upstreamReads := 0
	upstream := cfg.Upstream
	cfg.Upstream = func(ctx context.Context, repo string) (gitcache.Remote, error) {
		mu.Lock()
		upstreamReads++
		mu.Unlock()
		return upstream(ctx, repo)
	}
	closed, err := gitcache.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err = closed.Init(t.Context(), gittest.Repository); err == nil {
		t.Fatal("a closed cache cloned")
	}
	mu.Lock()
	reads := upstreamReads
	mu.Unlock()
	if started, _ := os.ReadFile(log); len(started) != 0 || reads != 0 {
		t.Fatal("a clone started after Close", string(started), reads)
	}
	for range 20 {
		c, err := gitcache.New(t.Context(), cfg)
		if err != nil {
			t.Fatal(err)
		}
		racing := make(chan error, 1)
		go func() { racing <- shortInit(t, c) }()
		if err = c.Close(); err != nil {
			t.Fatal(err)
		}
		<-racing
	}
	if _, err = os.Stat(filepath.Join(cfg.Root, "origin", "team", "widget.git", "HEAD")); !os.IsNotExist(err) {
		t.Fatal("a clone racing Close was published", err)
	}
}

func TestCloseEndsARunningFirstClone(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	_, _ = slowForge(t, f, &cfg)
	c, err := gitcache.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = shortInit(t, c); err == nil {
		t.Fatal("a gated clone finished")
	}
	if err = c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(cfg.Root, "origin", "team", "widget.git", "HEAD")); !os.IsNotExist(err) {
		t.Fatal("a clone ended by Close was published", err)
	}
}
