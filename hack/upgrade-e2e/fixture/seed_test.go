package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func TestSeedPriorityRunsHaveRunnablePipelines(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "oberth.sqlite")
	original := os.Args
	os.Args = []string{"fixture", "seed", path, "oberth.invalid/runner@sha256:" + strings.Repeat("a", 64)}
	t.Cleanup(func() { os.Args = original })
	seed(ctx, path)
	s, err := store.InspectCurrent(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	runs, err := s.ListActiveRuns(ctx, model.RunListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 5 {
		t.Fatalf("active runs=%d, want 5", len(runs))
	}
	jobs := &app.ArgoJobs{}
	for _, r := range runs {
		if r.Status != model.RunQueued {
			continue
		}
		size, err := jobs.PipelineSize(ctx, service.JobRequest{Run: r, SourceDir: filepath.Join(root, "fixtures", "sample")})
		if err != nil || string(size) != "M" {
			t.Fatalf("%s pipeline size=%q err=%v", r.Priority, size, err)
		}
	}
	upstream := filepath.Join(root, "fixtures", "sample.git")
	if output, err := exec.Command("/usr/bin/git", "-C", upstream, "rev-parse", "--is-bare-repository").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatalf("trusted-policy upstream is not a bare repository: %s %v", output, err)
	}
}
