package gitcache

import (
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

func TestStageReportsItsExportCapacityWait(t *testing.T) {
	synctest.Test(t, checkStageExportWait)
}

func checkStageExportWait(t *testing.T) {
	f := newExportWaitFixture(t)
	c, b := f.cache, f.binding
	c.export <- struct{}{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		stage, err := c.Stage(t.Context(), b)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = stage.Body.Close() }()
		if !stage.Waited {
			t.Error("export wait was not reported for authorization recheck")
		}
	}()
	synctest.Wait()
	<-c.export
	<-done
}

func TestStageReportsItsExportWaitBeforeAnArchiveHit(t *testing.T) {
	synctest.Test(t, checkStageHitAfterExportWait)
}

func checkStageHitAfterExportWait(t *testing.T) {
	f := newExportWaitFixture(t)
	if err := f.cache.lock(t.Context(), f.path); err != nil {
		t.Fatal(err)
	}
	unlock := sync.OnceFunc(func() { f.cache.unlock(f.path) })
	defer unlock()
	first, second := make(chan exportWaitResult, 1), make(chan exportWaitResult, 1)
	load := func(result chan<- exportWaitResult) {
		stage, err := f.cache.Stage(t.Context(), f.binding)
		result <- exportWaitResult{stage: stage, err: err}
	}
	// The first export owns the compression slot and waits for the repository.
	go load(first)
	synctest.Wait()
	// The second has missed the archive and now waits for that export slot.
	go load(second)
	synctest.Wait()
	unlock()
	a, b := <-first, <-second
	for _, result := range []exportWaitResult{a, b} {
		if result.stage.Body != nil {
			defer func() { _ = result.stage.Body.Close() }()
		}
		if result.err != nil {
			t.Fatal(result.err)
		}
	}
	if a.stage.Body.(*stagedReader).Name() != b.stage.Body.(*stagedReader).Name() {
		t.Fatal("parallel load rebuilt the archive after its export wait")
	}
	if !b.stage.Waited {
		t.Fatal("cached export wait was not reported for authorization recheck")
	}
}

type exportWaitResult struct {
	stage sourcedelivery.Staged
	err   error
}

type exportWaitFixture struct {
	cache   *Cache
	binding sourcecontract.Binding
	path    string
}

func newExportWaitFixture(t *testing.T) exportWaitFixture {
	t.Helper()
	binary, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	c, err := New(t.Context(), Config{Root: t.TempDir(), Scratch: t.TempDir(), Binary: binary})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	p, err := c.path("origin/team/widget")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(p, 0700); err != nil {
		t.Fatal(err)
	}
	git := func(input string, args ...string) string {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), binary, append([]string{"-C", p, "-c", "core.hooksPath=/dev/null", "-c", "user.name=source", "-c", "user.email=source@example.test"}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	git("", "init", "--bare")
	tree := git("", "hash-object", "-w", "-t", "tree", "--stdin")
	commit := git("source\n", "commit-tree", tree)
	b := sourcecontract.Binding{Template: "build", Artifact: "source", Path: "/work/src", RepositoryID: 1,
		Revision:   sourcecontract.Revision{Repository: "origin/team/widget", Commit: commit},
		Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}}
	return exportWaitFixture{cache: c, binding: b, path: p}
}
