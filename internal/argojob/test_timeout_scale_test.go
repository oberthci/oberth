package argojob

import (
	"os/exec"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestOberthCITestTimeoutScale(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "build.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	pending := map[string]bool{"test-store": true, "test-service": true, "test-rest": true, "test-gitcache": true}
	for _, template := range workflow.Spec.Templates {
		if !pending[template.Name] {
			continue
		}
		if template.Container == nil {
			t.Fatalf("%s has no test container", template.Name)
		}
		found := 0
		for _, entry := range template.Container.Env {
			if entry.Name == "OBERTH_TEST_TIMEOUT_SCALE" {
				found++
				if entry.Value != "4" || entry.ValueFrom != nil {
					t.Fatalf("%s must run its test fixtures at scale 4", template.Name)
				}
			}
		}
		if found != 1 {
			t.Fatalf("%s has %d timeout scale entries, want one", template.Name, found)
		}
		delete(pending, template.Name)
	}
	if len(pending) != 0 {
		t.Fatalf("missing test leaves: %v", pending)
	}

	workflow, err = argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range workflow.Spec.Templates {
		if template.Name != "release-test" {
			continue
		}
		if template.Container == nil || len(template.Container.Args) < 2 {
			t.Fatal("release-test lost its clean-environment wrapper")
		}
		// Execute the actual env -i wrapper: setting the Kubernetes env alone
		// would silently lose the scale before the Go test binary starts.
		command := exec.CommandContext(t.Context(), "/bin/sh", "-c", template.Container.Args[0], "fixture", "/bin/sh", "-c", `test "$OBERTH_TEST_TIMEOUT_SCALE" = 4`)
		command.Env = []string{"OBERTH_RELEASE_SHA=fixture", "OBERTH_RELEASE_TAG=v0.0.0", "OBERTH_TEST_TIMEOUT_SCALE=1"}
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("release-test child lost scale 4: %v: %s", err, output)
		}
		return
	}
	t.Fatal("missing release-test leaf")
}
