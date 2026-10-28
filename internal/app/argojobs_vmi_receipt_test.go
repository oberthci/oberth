package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"

	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

type vmiReceiptFragments struct {
	t     *testing.T
	calls int
}

func (f *vmiReceiptFragments) Load(_ context.Context, key argoworkflow.FragmentKey, tier periapsis.Trigger, ref model.RefKind, trigger string) (argoworkflow.Fragment, error) {
	f.t.Helper()
	f.calls++
	if tier != periapsis.TriggerCI || ref != model.RefBranch || (trigger != "branch" && trigger != "push" && trigger != "schedule") {
		f.t.Fatalf("untrusted fragment loading context: %s %s %s", tier, ref, trigger)
	}
	name := strings.TrimPrefix(key.Repo, "example/")
	body, err := os.ReadFile("../../pkg/argoworkflow/testdata/vmi-run-" + name + "-fragment.yaml")
	return argoworkflow.Fragment{Key: key, SHA: strings.Repeat("a", 40), Source: body}, err
}

func appVMIReceiptFixture(t *testing.T, trigger string) (*ArgoJobs, *routeController, *vmiReceiptFragments, service.JobRequest) {
	t.Helper()
	source, err := os.ReadFile("../../pkg/argoworkflow/testdata/vmi-run-projection.yaml")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	writePipeline(t, dir, "build.yaml", string(source))
	controller := &routeController{}
	fragments := &vmiReceiptFragments{t: t}
	jobs := &ArgoJobs{controller: controller, auditor: &stubAuditor{}, secretAccess: emptySecretAccess{}, fragments: fragments, intents: map[string]argoIntent{},
		config: argojob.Config{Namespace: "pipelines", PipelineServiceAccount: "pipeline", CredentialedServiceAccount: "release", CISecretsServiceAccount: "ci", ExecutorServiceAccount: "executor", VMLauncherServiceAccount: "vm-launcher", ContainerDiskPrefixes: []string{"quay.io/example/"}, RunnerImagePrefixes: []string{"busybox:"}},
		kubeVirtGrants: kubeVirtGrantFunc(func(context.Context, int64) (store.KubeVirtBudget, bool, error) {
			return store.KubeVirtBudget{MaxVMs: 4, MaxCores: 12, MaxMemoryBytes: 24 << 30}, true, nil
		}),
	}
	request := service.JobRequest{JobName: "workflow", Run: model.Run{ID: "run", RepoID: 7, Trigger: trigger, RefKind: model.RefBranch, Ref: "main", SHA: strings.Repeat("b", 40), Actor: "agent@example"}, Repository: model.Repository{ID: 7, Name: "repo"}, SourceDir: dir}
	return jobs, controller, fragments, request
}

func TestVMIDemandReceiptConnectsReservationToSubmission(t *testing.T) {
	for _, trigger := range []string{"branch", "push", "schedule"} {
		t.Run(trigger, func(t *testing.T) {
			jobs, controller, fragments, request := appVMIReceiptFixture(t, trigger)
			resources, err := jobs.PipelineKubeVirt(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			if resources.Demand != (argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}) || resources.Receipt == nil {
				t.Fatalf("resources=%+v", resources)
			}
			request.VMIDemandReceipt = resources.Receipt
			if err := jobs.CreateCI(t.Context(), request); err != nil {
				t.Fatal(err)
			}
			if fragments.calls != 2 {
				t.Fatalf("submission re-resolved a tag after reservation: %d loads", fragments.calls)
			}
			if controller.request.VMIDemandReceipt != resources.Receipt {
				t.Fatal("receipt was dropped")
			}
			wf, err := argojob.Build(jobs.config, controller.request)
			if err != nil {
				t.Fatal(err)
			}
			var quota struct {
				Peak    argoworkflow.VMIDemand        `json:"peak"`
				Receipt argoworkflow.VMIDemandSummary `json:"receipt"`
			}
			if err := json.Unmarshal([]byte(wf.Annotations["oberth.ci/vmi-quota"]), &quota); err != nil {
				t.Fatal(err)
			}
			if quota.Peak != resources.Demand || quota.Receipt != resources.Receipt.Summary() {
				t.Fatalf("reservation/submission graphs differ: %+v", quota)
			}
			resolved, err := argoworkflow.ResolveRunSource(controller.request.Source, controller.request.Fragments, quota.Receipt.Binding)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(vmiWhenStrings(resolved.Workflow()), vmiWhenStrings(wf)) {
				t.Fatal("submission rewrote an authored when string")
			}
			kind, _ := runKind(request.Run)
			for _, p := range wf.Spec.Arguments.Parameters {
				if p.Name == argoworkflow.TriggerParameter && p.Value.String() != string(kind) {
					t.Fatalf("YAML spoofed trigger: %+v", p)
				}
			}
			// The cleanup DAG, including terminal-state OR expressions, and the
			// cancellation-only onExit survive sizing and final admission intact.
			if wf.Spec.OnExit != "cleanup" {
				t.Fatal("onExit changed")
			}
			found := false
			for _, tpl := range wf.Spec.Templates {
				if tpl.Name == "phases" {
					for _, task := range tpl.DAG.Tasks {
						if strings.HasPrefix(task.Name, "delete-") && strings.Contains(task.Depends, ".Failed ||") {
							found = true
						}
					}
				}
			}
			if !found {
				t.Fatal("failure-cleanup dependency was rewritten")
			}
		})
	}
}

func vmiWhenStrings(wf *wfv1.Workflow) map[string]string {
	out := map[string]string{}
	var visit func(*wfv1.Template, string)
	visit = func(t *wfv1.Template, path string) {
		if t == nil {
			return
		}
		if t.DAG != nil {
			for _, task := range t.DAG.Tasks {
				key := path + "/dag/" + task.Name
				out[key] = task.When
				visit(task.Inline, key+"/inline")
			}
		}
		for i, group := range t.Steps {
			for _, step := range group.Steps {
				key := fmt.Sprintf("%s/steps/%d/%s", path, i, step.Name)
				out[key] = step.When
				visit(step.Inline, key+"/inline")
			}
		}
	}
	for i := range wf.Spec.Templates {
		visit(&wf.Spec.Templates[i], wf.Spec.Templates[i].Name)
	}
	return out
}

func TestRawPartitionGuardUsesSumWithoutRewriting(t *testing.T) {
	jobs, controller, _, request := appVMIReceiptFixture(t, "schedule")
	source, err := jobs.readRunSource(request)
	if err != nil {
		t.Fatal(err)
	}
	wf, err := argoworkflow.Decode(source)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wf.Spec.Templates {
		if wf.Spec.Templates[i].Name == "partitions" {
			for j := range wf.Spec.Templates[i].DAG.Tasks {
				wf.Spec.Templates[i].DAG.Tasks[j].When = fmt.Sprintf("{{inputs.parameters.partition}} == %d", j)
			}
		}
	}
	source, err = json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	writePipeline(t, request.SourceDir, "build.yaml", string(source))
	resources, err := jobs.PipelineKubeVirt(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	if resources.Demand.VMs != 4 {
		t.Fatalf("raw partition guards earned credits: %+v", resources)
	}
	request.VMIDemandReceipt = resources.Receipt
	if err := jobs.CreateCI(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	submitted, err := argojob.Build(jobs.config, controller.request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(vmiWhenStrings(wf), vmiWhenStrings(submitted)) {
		t.Fatal("raw authored when was rewritten")
	}
}

func TestVMIDemandReceiptPreservesGrantBackedKVMOnlySteps(t *testing.T) {
	jobs, _, _, request := appVMIReceiptFixture(t, "branch")
	wf, err := argoworkflow.Decode([]byte(strings.ReplaceAll(groupedPipeline("    oberth.ci/kvm-templates: main\n"), "golang:1.26-alpine", "busybox:1.37")))
	if err != nil {
		t.Fatal(err)
	}
	wf.Spec.Templates[0].Metadata.Annotations = map[string]string{argoworkflow.WorkspaceMountsAnnotation: "none", argoworkflow.WorkspaceEnvAnnotation: "none"}
	disabled := false
	wf.Spec.Templates[0].AutomountServiceAccountToken = &disabled
	source, err := json.Marshal(wf)
	if err != nil {
		t.Fatal(err)
	}
	writePipeline(t, request.SourceDir, "build.yaml", string(source))
	resources, err := jobs.PipelineKubeVirt(t.Context(), request)
	if err != nil || resources.Demand != (argoworkflow.VMIDemand{}) || resources.Receipt == nil {
		t.Fatalf("KVM-only run received VMI demand or lost its own grant: %+v %v", resources, err)
	}
	jobs.kubeVirtGrants = nil
	if _, err := jobs.PipelineKubeVirt(t.Context(), request); err == nil {
		t.Fatal("KVM-only admission skipped its required grant")
	}
}

func TestVMIDemandReceiptRefusesChangedSubmission(t *testing.T) {
	for _, kind := range []string{"source", "durable-trigger", "fragment-content", "zero-to-vmi"} {
		t.Run(kind, func(t *testing.T) {
			jobs, controller, _, request := appVMIReceiptFixture(t, "branch")
			if kind == "zero-to-vmi" {
				writePipeline(t, request.SourceDir, "build.yaml", strings.ReplaceAll(groupedPipeline(""), "golang:1.26-alpine", "busybox:1.37"))
			}
			resources, err := jobs.PipelineKubeVirt(t.Context(), request)
			if err != nil {
				t.Fatal(err)
			}
			request.VMIDemandReceipt = resources.Receipt
			switch kind {
			case "source":
				source, err := jobs.readRunSource(request)
				if err != nil {
					t.Fatal(err)
				}
				writePipeline(t, request.SourceDir, "build.yaml", string(source)+"\n")
			case "durable-trigger":
				request.Run.Trigger = "schedule"
			case "zero-to-vmi":
				source, err := os.ReadFile("../../pkg/argoworkflow/testdata/vmi-run-projection.yaml")
				if err != nil {
					t.Fatal(err)
				}
				writePipeline(t, request.SourceDir, "build.yaml", string(source))
			case "fragment-content":
				if err := jobs.CreateCI(t.Context(), request); err != nil {
					t.Fatal(err)
				}
				submission := controller.request
				submission.Fragments = resources.Receipt.Fragments()
				for key, fragment := range submission.Fragments {
					fragment.Source = append(fragment.Source, '\n')
					submission.Fragments[key] = fragment
					break
				}
				if _, err := argojob.Build(jobs.config, submission); err == nil {
					t.Fatal("changed fragment was admitted under the old reservation")
				}
				return
			}
			if err := jobs.CreateCI(t.Context(), request); err == nil {
				t.Fatal("changed submission was admitted")
			}
			if controller.request.Source != nil {
				t.Fatal("changed submission reached create")
			}
		})
	}
}
