package gitcache_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"k8s.io/utils/clock"
)

func TestArchiveHitBatchesRecordedTags(t *testing.T) {
	for _, count := range []int{0, 50, 300} {
		t.Run(fmt.Sprint(count), func(t *testing.T) { checkArchiveTagBatch(t, count) })
	}
}

func checkArchiveTagBatch(t *testing.T, count int) {
	t.Helper()
	f := gittest.New(t)
	real := f.Binary
	commands := filepath.Join(f.Root, "archive-commands.jsonl")
	wrapper := filepath.Join(f.Root, "archive-git")
	program := fmt.Sprintf("#!/usr/bin/python3\nimport json, os, sys\nwith open(%q, 'a') as log: log.write(json.dumps(sys.argv[1:])+'\\n')\nos.execv(%q, [%q]+sys.argv[1:])\n", commands, real, real)
	native := os.Getenv("PERF_TAG_NATIVE") == "1"
	if !native {
		if err := os.WriteFile(wrapper, []byte(program), 0700); err != nil {
			t.Fatal(err)
		}
		f.Binary = wrapper
	}
	c := f.Cache(t, nil)
	b := f.Binding()
	var updates strings.Builder
	for i := range count {
		ref := sourcecontract.Ref{Name: fmt.Sprintf("refs/tags/recorded-%03d", i), ObjectSHA: f.Commit, CommitSHA: f.Commit}
		b.Export.Tags = append(b.Export.Tags, ref)
		fmt.Fprintf(&updates, "update %s %s\n", ref.Name, ref.ObjectSHA)
	}
	cmd := exec.CommandContext(t.Context(), real, "--git-dir="+f.Store(), "update-ref", "--stdin")
	cmd.Stdin = strings.NewReader(updates.String())
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatal(err, string(output))
	}
	first, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(commands, nil, 0600); err != nil {
		t.Fatal(err)
	}
	start := (clock.RealClock{}).Now()
	stage, err := c.Stage(t.Context(), b)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = stage.Body.Close() }()
	elapsed := (clock.RealClock{}).Since(start)
	if native {
		t.Logf("%d recorded tags: native warm stage %v", count, elapsed)
		return
	}
	data, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	calls := strings.Count(string(data), `"cat-file"`)
	t.Logf("%d recorded tags: warm stage %v, %d object-check processes", count, elapsed, calls)
	if calls > 2 {
		t.Fatal("archive hit started Git for each recorded tag", calls)
	}
}
