package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
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
	for _, r := range runs {
		if r.Status != model.RunQueued {
			continue
		}
		file, trigger := "build.yaml", argoworkflow.RunTriggerBranch
		if r.Release {
			file, trigger = "release.yaml", argoworkflow.RunTriggerTag
		}
		// Decode the same document and resource contract the scheduler reads.
		// This fixture is also compiled against earlier published releases.
		source, err := os.ReadFile(filepath.Join(root, "fixtures", "sample", ".oberth", file))
		if err != nil {
			t.Fatalf("%s pipeline source: %v", r.Priority, err)
		}
		workflow, err := argoworkflow.Decode(source)
		if err != nil {
			t.Fatal(err)
		}
		size, err := argoworkflow.DeclaredSizeFor(workflow, trigger)
		if err != nil || string(size) != "M" {
			t.Fatalf("%s pipeline size=%q err=%v", r.Priority, size, err)
		}
		if len(workflow.Spec.Templates) != 1 {
			t.Fatalf("%s templates=%d, want one runnable entrypoint", r.Priority, len(workflow.Spec.Templates))
		}
		entry := workflow.Spec.Templates[0]
		if entry.Name != workflow.Spec.Entrypoint || entry.Container == nil || entry.Container.Image != os.Args[3] || strings.Join(entry.Container.Command, " ") != "/usr/local/bin/oberth version" {
			t.Fatalf("%s entrypoint must run the signed release binary: %+v", r.Priority, entry)
		}
	}
	upstream := filepath.Join(root, "fixtures", "sample.git")
	if output, err := exec.Command("/usr/bin/git", "-C", upstream, "rev-parse", "--is-bare-repository").CombinedOutput(); err != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatalf("trusted-policy upstream is not a bare repository: %s %v", output, err)
	}
}
