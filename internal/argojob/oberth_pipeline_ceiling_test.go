package argojob

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/pipeline"
)

// Check authored containers, scripts, container sets, defaults, inline calls,
// init containers and sidecars, including unused definitions, before injection.
func TestOberthPipelinesFitLCeiling(t *testing.T) {
	for _, file := range []string{"build.yaml", "release.yaml"} {
		t.Run(file, func(t *testing.T) {
			workflow, err := argoworkflow.Decode(loadPipeline(t, file))
			if err != nil {
				t.Fatal(err)
			}
			if err = argoworkflow.ValidateResourceCeilingWithCapability(workflow, "oberth", pipeline.L, 86400, nil); err != nil {
				t.Fatalf("every pipeline leaf must fit 4 CPU / 8Gi: %v", err)
			}
		})
	}
}

// Execute the real clean-environment bootstrap with a harmless observer. This
// checks that background task/heap bounds reach Go after inherited environment is wiped.
func TestOberthReleaseGoWorkersAreBounded(t *testing.T) {
	workflow, err := argoworkflow.Decode(loadPipeline(t, "release.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"release-test-store", "release-test-service", "release-test-rest", "release-test-gitcache", "release-build"} {
		t.Run(name, func(t *testing.T) {
			for _, leaf := range workflow.Spec.Templates {
				if leaf.Name != name {
					continue
				}
				container := leaf.Container
				if container == nil || len(container.Args) == 0 {
					t.Fatal("missing Go bootstrap")
				}
				cmd := exec.CommandContext(t.Context(), "/bin/sh", "-c", container.Args[0], "budget-check", "/usr/bin/env")
				cmd.Env = []string{"PATH=/usr/bin:/bin", "OBERTH_RELEASE_SHA=" + strings.Repeat("a", 40), "OBERTH_RELEASE_TAG=v1.0.0", "GOMAXPROCS=64", "GOFLAGS=-p=64", "GOMEMLIMIT=64GiB"}
				output, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("bootstrap: %v: %s", err, output)
				}
				env := map[string]string{}
				for _, line := range strings.Split(string(output), "\n") {
					key, value, ok := strings.Cut(line, "=")
					if ok {
						env[key] = value
					}
				}
				memoryLimit := "1GiB"
				if limit, ok := map[string]string{"release-test-store": "2GiB", "release-test-service": "2GiB", "release-test-rest": "4GiB", "release-test-gitcache": "256MiB"}[name]; ok {
					memoryLimit = limit
				}
				for key, want := range map[string]string{"GOMAXPROCS": "2", "GOFLAGS": "-p=1", "GOMEMLIMIT": memoryLimit} {
					if env[key] != want {
						t.Errorf("%s=%q, want %q after clean bootstrap", key, env[key], want)
					}
				}
				return
			}
			t.Fatal("missing release Go leaf")
		})
	}
}
