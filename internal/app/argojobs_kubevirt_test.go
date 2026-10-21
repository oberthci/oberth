package app

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type kubeVirtGrantFunc func(context.Context, int64) (store.KubeVirtBudget, bool, error)

func (load kubeVirtGrantFunc) ActiveKubeVirtGrant(ctx context.Context, id int64) (store.KubeVirtBudget, bool, error) {
	return load(ctx, id)
}

func demandPipeline(cores int, memory string) string {
	return groupedPipeline("") + fmt.Sprintf(`    - name: vm
      resource:
        action: create
        manifest: |
          apiVersion: kubevirt.io/v1
          kind: VirtualMachineInstance
          spec:
            domain:
              cpu:
                cores: %d
              resources:
                requests:
                  memory: %s
`, cores, memory)
}

func TestPipelineKubeVirtUsesOwnDocumentAndCappedGrant(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", demandPipeline(2, "1Gi"))
	writePipeline(t, dir, "release.yaml", demandPipeline(3, "2Gi"))
	jobs := &ArgoJobs{
		config: argojob.Config{KubeVirtCeiling: argoworkflow.VMIBudget{MaxVMs: 3, MaxCores: 8, MaxMemory: resource.MustParse("8Gi")}},
		kubeVirtGrants: kubeVirtGrantFunc(func(_ context.Context, repoID int64) (store.KubeVirtBudget, bool, error) {
			if repoID != 7 {
				t.Fatalf("grant read for repo %d", repoID)
			}
			return store.KubeVirtBudget{MaxVMs: 5, MaxCores: 12, MaxMemoryBytes: 24 << 30}, true, nil
		}),
	}
	for _, run := range []model.Run{{Ref: "feature", Trigger: "branch"}, {Ref: "promotion/main", Trigger: "promotion"}, {Ref: "v1", Trigger: "tag", Release: true}} {
		resources, err := jobs.PipelineKubeVirt(t.Context(), service.JobRequest{Run: run, Repository: model.Repository{ID: 7}, SourceDir: dir})
		want := argoworkflow.VMIDemand{VMs: 1, Cores: 2, MemoryBytes: 1 << 30}
		if run.Release {
			want.Cores, want.MemoryBytes = 3, 2<<30
		}
		if err != nil || resources.Demand != want || resources.Limit != (argoworkflow.VMIDemand{VMs: 3, Cores: 8, MemoryBytes: 8 << 30}) {
			t.Fatalf("%s resources = %#v, %v", run.Trigger, resources, err)
		}
	}
}

func TestPipelineKubeVirtRefusesMissingGrantAndOversizedDemand(t *testing.T) {
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", demandPipeline(7, "1Gi"))
	request := service.JobRequest{Run: model.Run{Ref: "main"}, Repository: model.Repository{ID: 1, Name: "vm-repo"}, SourceDir: dir}
	jobs := &ArgoJobs{}
	if _, err := jobs.PipelineKubeVirt(t.Context(), request); err == nil || !strings.Contains(err.Error(), "no KubeVirt grant") {
		t.Fatalf("missing grant = %v", err)
	}
	jobs.kubeVirtGrants = kubeVirtGrantFunc(func(context.Context, int64) (store.KubeVirtBudget, bool, error) {
		return store.KubeVirtBudget{}, true, nil
	})
	if _, err := jobs.PipelineKubeVirt(t.Context(), request); err == nil || !strings.Contains(err.Error(), "VMI run budget exceeded") {
		t.Fatalf("oversized demand = %v", err)
	}
	writePipeline(t, dir, "build.yaml", groupedPipeline(""))
	jobs.kubeVirtGrants = nil
	if resources, err := jobs.PipelineKubeVirt(t.Context(), request); err != nil || resources != (service.KubeVirtResources{}) {
		t.Fatalf("ordinary pipeline = %#v, %v", resources, err)
	}
}

func TestAdoptedKubeVirtDemandUsesVerifiedBindingWithoutGrant(t *testing.T) {
	want := argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 8 << 30}
	controller := &adoptingArgoControl{found: true, binding: argojob.Adoption{Size: periapsis.M, KubeVirt: want}}
	jobs := &ArgoJobs{controller: controller, intents: map[string]argoIntent{}}
	run := model.Run{ID: "run", JobName: "workflow", Trigger: "branch"}
	if _, err := jobs.AdoptedKubeVirtDemand(run); err == nil {
		t.Fatal("demand returned before Workflow verification")
	}
	if _, found, err := jobs.Adopt(t.Context(), run); err != nil || !found {
		t.Fatalf("adopt = %v, %v", found, err)
	}
	if got, err := jobs.AdoptedKubeVirtDemand(run); err != nil || got != want {
		t.Fatalf("demand without active grant = %#v, %v", got, err)
	}
	foreign := run
	foreign.ID = "foreign"
	if _, err := jobs.AdoptedKubeVirtDemand(foreign); err == nil {
		t.Fatal("foreign run read adopted demand")
	}
}
