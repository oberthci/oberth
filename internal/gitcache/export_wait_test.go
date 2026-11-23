package gitcache

import (
	"os"
	"os/exec"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/sourcecontract"
)

func TestStageReportsItsExportCapacityWait(t *testing.T) {
	synctest.Test(t, checkStageExportWait)
}

func checkStageExportWait(t *testing.T) {
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
