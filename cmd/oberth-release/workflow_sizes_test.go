package main

import (
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestOwnPipelinesUseRequestsForCapacity(t *testing.T) {
	for _, name := range []string{"build", "release", "walk"} {
		t.Run(name, func(t *testing.T) {
			raw := ownWorkflow(t, name)
			if strings.Contains(string(raw), "semaphores:") || strings.Contains(string(raw), "oberth-sync-limits") {
				t.Fatal("own pipeline holds shared lock sizes")
			}
			assertPipelineRequests(t, raw)
		})
	}
	raw, err := os.ReadFile(buildResourcesFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "oberth-sync-limits") {
		t.Fatal("pipeline resources carry lock sizes")
	}
}

func assertPipelineRequests(t *testing.T, raw []byte) {
	t.Helper()
	w, err := argoworkflow.Decode(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range w.Spec.Templates {
		if template.Container == nil {
			continue
		}
		resources := template.Container.Resources
		if len(resources.Requests) == 0 {
			resources = w.Spec.TemplateDefaults.Container.Resources
		}
		if resources.Requests.Memory().IsZero() || resources.Requests.Cpu().IsZero() || resources.Requests.Memory().Cmp(*resources.Limits.Memory()) != 0 {
			t.Fatal("container must request its memory bound and CPU", template.Name, resources)
		}
	}
}

func TestReleasePublicationUsesRepositoryMutex(t *testing.T) {
	w, err := argoworkflow.Decode(ownWorkflow(t, "release"))
	if err != nil {
		t.Fatal(err)
	}
	for _, template := range w.Spec.Templates {
		publish := template.Name == "release-published-immutable" || template.Name == "latest-advanced-monotonic"
		s := template.Synchronization
		locked := s != nil && len(s.Mutexes) == 1 && s.Mutexes[0].Name == "github/oberthci/oberth/release-publish" && s.Mutexes[0].Namespace == "" && !s.Mutexes[0].Database
		if publish != locked || s != nil && len(s.Semaphores) != 0 {
			t.Fatal("publication mutex differs", template.Name, s)
		}
	}
	r := ownRequest(ownWorkflow(t, "release"), "release", model.TriggerTag)
	r.Grants = releaseGrants(r)
	p := ownPolicy()
	p.Release = true
	if _, err := argoworkflow.Prepare(t.Context(), r, p, ownHooks(t, r)); err != nil {
		t.Fatal("publication mutex refused for its repository", err)
	}
}
