package argojob

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

func TestAdoptionPreservesRunAwareVMIReceipt(t *testing.T) {
	read := func(name string) []byte {
		b, err := os.ReadFile("../../pkg/argoworkflow/testdata/" + name)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	request := testRequest(periapsis.TriggerCI, string(read("vmi-run-projection.yaml")))
	request.RunTrigger = argoworkflow.RunTriggerSchedule
	request.ScheduleEntry = "nightly"
	request.PipelineFile = argoworkflow.BuildFile
	request.KubeVirtGranted = true
	request.Fragments = map[argoworkflow.FragmentKey]argoworkflow.Fragment{}
	for _, name := range []string{"toolchain", "driver"} {
		key := argoworkflow.FragmentKey{Repo: "example/" + name, Version: "v1"}
		request.Fragments[key] = argoworkflow.Fragment{Key: key, SHA: strings.Repeat("d", 40), Source: read("vmi-run-" + name + "-fragment.yaml")}
	}
	config := testConfig()
	config.VMLauncherServiceAccount = "vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	config.RunnerImagePrefixes = []string{"busybox:"}
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	wf.UID = "workflow-uid"
	controller := &Controller{config: config, workflows: &staticWorkflowClient{workflow: wf}}
	adopt := AdoptionRequest{Name: wf.Name, RunID: request.RunID, Ref: request.Ref, SHA: request.SHA, Trigger: request.Trigger, RunTrigger: request.RunTrigger, ScheduleEntry: request.ScheduleEntry}
	want := argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}
	binding, found, err := controller.Adopt(t.Context(), adopt)
	if err != nil || !found || binding.KubeVirt != want {
		t.Fatalf("adoption=%+v found=%v err=%v", binding, found, err)
	}
	// A receipt-less older submission keeps the conservative legacy check.
	var quota vmiQuotaBinding
	if err := json.Unmarshal([]byte(wf.Annotations[vmiQuotaAnnotation]), &quota); err != nil {
		t.Fatal(err)
	}
	if quota.Receipt == nil || quota.Peak != want {
		t.Fatalf("receipt not sealed in quota: %+v", quota)
	}
	for _, kind := range []string{"run", "repo", "sha", "trigger", "schedule", "source-hash", "graph-hash", "demand", "version", "missing"} {
		t.Run(kind, func(t *testing.T) {
			copy := wf.DeepCopy()
			altered := quota
			summary := *quota.Receipt
			altered.Receipt = &summary
			switch kind {
			case "run":
				summary.Binding.RunID += "-foreign"
			case "repo":
				summary.Binding.RepositoryID++
			case "sha":
				summary.Binding.SHA = strings.Repeat("f", 40)
			case "trigger":
				summary.Binding.Trigger = argoworkflow.RunTriggerBranch
			case "schedule":
				summary.Binding.ScheduleEntry = "other"
			case "source-hash":
				summary.SourceSHA256 = "invalid"
			case "graph-hash":
				summary.GraphSHA256 = "invalid"
			case "demand":
				summary.Demand.VMs++
			case "version":
				summary.Version++
			case "missing":
				altered.Receipt = nil
			}
			encoded, err := json.Marshal(altered)
			if err != nil {
				t.Fatal(err)
			}
			copy.Annotations[vmiQuotaAnnotation] = string(encoded)
			controller.workflows = &staticWorkflowClient{workflow: copy}
			if _, found, err := controller.Adopt(t.Context(), adopt); found || !errors.Is(err, ErrAdoptionRefused) {
				t.Fatalf("changed receipt adopted: %v %v", found, err)
			}
		})
	}
}
