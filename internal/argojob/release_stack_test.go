package argojob

import (
	"encoding/json"
	"os"
	"runtime"
	"slices"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

// The release combines two last-stage Build transformations. A measured driver
// must retain inherited selectors while VMI resource Pods retain the create
// guard and the proven two-VM peak, all sealed in the final adoption identity.
func TestReleaseStackPreservesVMIQuotaAndStepUsage(t *testing.T) {
	source, err := os.ReadFile("../../pkg/argoworkflow/testdata/vmi-phases.yaml")
	if err != nil {
		t.Fatal(err)
	}
	config := testConfig()
	config.VMLauncherServiceAccount = "vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	config.RunnerImagePrefixes = []string{"busybox:"}
	request := testRequest(periapsis.TriggerCI, string(source))
	request.KubeVirtGranted = true
	request.SourceVolume = SourceVolume{ClaimName: sourceClaimName(request.Name), SubPath: "src", ArtifactsSubPath: artifactsSubPath, BinarySubPath: binarySubPath}
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	var binding vmiQuotaBinding
	if err := json.Unmarshal([]byte(wf.Annotations[vmiQuotaAnnotation]), &binding); err != nil {
		t.Fatal(err)
	}
	want := argoworkflow.VMIDemand{VMs: 2, Cores: 4, MemoryBytes: 12 << 30}
	if binding.RepositoryID != request.RepositoryID || binding.Peak != want || !binding.Peak.Fits(binding.Limit) {
		t.Fatalf("quota binding lost: %+v", binding)
	}
	creates, measured := 0, 0
	walkTemplates(wf, func(template *wfv1.Template) {
		// Simulate the Pod selector resolved from administrator workflowDefaults;
		// repositories cannot author spec.nodeSelector themselves.
		inherited := corev1.PodSpec{NodeSelector: map[string]string{"pool": "ci"}}
		if template.Resource != nil && template.Resource.Action == "create" {
			inherited.Containers = []corev1.Container{{Name: "main", Command: []string{"argoexec", "resource", "create"}}}
			patched := applyCredentialWaitPatch(t, inherited, template.PodSpecPatch)
			if len(patched.Containers) == 0 || !guardedVMIGetCommand(patched.Containers[0].Command, patched.Containers[0].Args) {
				t.Fatalf("VMI guard lost: %+v", patched.Containers)
			}
			creates++
		}
		if c := usageContainer(template); c != nil && len(c.Command) >= 3 && slices.Equal(c.Command[:3], []string{usageBinPath, usageCommand, "--"}) {
			patched := applyCredentialWaitPatch(t, inherited, template.PodSpecPatch)
			if patched.NodeSelector["pool"] != "ci" || patched.NodeSelector[corev1.LabelArchStable] != runtime.GOARCH || len(template.NodeSelector) != 0 {
				t.Fatalf("measured step scheduling escaped: %+v", patched.NodeSelector)
			}
			measured++
		}
	})
	if creates != 4 || measured != 1 {
		t.Fatalf("creates=%d measured templates=%d", creates, measured)
	}
	identity, err := specIdentity(wf)
	if err != nil || wf.Annotations[identityAnnotation] != identity {
		t.Fatalf("combined transformations were not sealed: %v", err)
	}
}
