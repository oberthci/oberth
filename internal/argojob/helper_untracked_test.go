package argojob

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRerunNamesRetainedUntrackedFiles(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	for _, name := range []string{"notes.txt", "line\nbreak"} {
		if err := os.WriteFile(filepath.Join(r.sourceRoot, name), []byte("kept"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	bundle, digest := rerunBundle(t, minimalRerunFiles(), nil, strings.Repeat("f", 40))
	stage, err := r.stageRerun(t.Context(), bytes.NewReader(bundle), digest)
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(stage)
	r.command, r.streams = []string{"true"}, HoldIO{}
	var diagnostic bytes.Buffer
	if code := r.rerunCommand(t.Context(), holdRequest{Stage: stage, Digest: digest}, 7, &diagnostic); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if !strings.Contains(diagnostic.String(), `kept untracked files: "line\nbreak", "notes.txt"`) {
		t.Fatal("retained filenames missing", diagnostic.String())
	}
	if strings.Index(diagnostic.String(), "kept untracked") < strings.Index(diagnostic.String(), "rerunning on") {
		t.Fatal("retained list belongs to predecessor logs")
	}
}

func TestRetainedUntrackedListIsBounded(t *testing.T) {
	t.Parallel()
	r := rerunRuntime(t)
	for i := range 40 {
		if err := os.WriteFile(filepath.Join(r.sourceRoot, fmt.Sprintf("file-%02d", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(r.sourceRoot)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	got := keptUntracked(root, nil)
	if !strings.Contains(got, `"file-00"`) || strings.Contains(got, `"file-32"`) || !strings.HasSuffix(got, "(list truncated)") || len(got) > 4300 {
		t.Fatal("untracked list is unbounded", got)
	}
}
