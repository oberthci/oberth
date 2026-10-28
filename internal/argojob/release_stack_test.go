package argojob

import (
	"bytes"
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
	config.applyDefaults()
	source = bytes.Replace(source, []byte(`command: ["true"]`), []byte("command: [\"true\"]\n        resources:\n          limits: {memory: 256Mi}"), 1)
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
	creates, deletes, measured := 0, 0, 0
	walkTemplates(wf, func(template *wfv1.Template) {
		// Simulate the Pod selector resolved from administrator workflowDefaults;
		// repositories cannot author spec.nodeSelector themselves.
		inherited := corev1.PodSpec{NodeSelector: map[string]string{"pool": "ci"}}
		if template.Resource != nil {
			inherited.Containers = []corev1.Container{{Name: "main", Command: []string{"argoexec", "resource", template.Resource.Action}}}
			patched := applyCredentialWaitPatch(t, inherited, template.PodSpecPatch)
			if usageContainer(template) != nil || len(template.NodeSelector) != 0 || len(patched.NodeSelector) != 1 || patched.NodeSelector["pool"] != "ci" {
				t.Fatalf("resource %q was wrapped or gained a selector: template=%+v pod=%+v", template.Name, template.NodeSelector, patched.NodeSelector)
			}
			main := patched.Containers[0]
			if slices.Contains(main.Command, usageBinPath) || slices.ContainsFunc(main.VolumeMounts, func(m corev1.VolumeMount) bool { return m.MountPath == usageBinMountPath }) {
				t.Fatalf("resource %q gained the usage wrapper: %+v", template.Name, main)
			}
			switch template.Resource.Action {
			case "create":
				if !guardedVMIGetCommand(main.Command, main.Args) {
					t.Fatalf("VMI guard lost: %+v", main)
				}
				creates++
			case "delete":
				if !slices.Equal(main.Command, []string{"argoexec", "resource", "delete"}) || len(main.Args) != 0 {
					t.Fatalf("VMI delete command changed: %+v", main)
				}
				deletes++
			default:
				t.Fatalf("unexpected resource action %q", template.Resource.Action)
			}
		}
		if c := usageContainer(template); c != nil && len(c.Command) >= 3 && slices.Equal(c.Command[:3], []string{usageBinPath, usageCommand, "--"}) {
			patched := applyCredentialWaitPatch(t, inherited, template.PodSpecPatch)
			if patched.NodeSelector["pool"] != "ci" || patched.NodeSelector[corev1.LabelArchStable] != runtime.GOARCH || len(template.NodeSelector) != 0 {
				t.Fatalf("measured step scheduling escaped: %+v", patched.NodeSelector)
			}
			waiter := slices.IndexFunc(patched.InitContainers, func(c corev1.Container) bool { return c.Name == executorTokenWaiter })
			init := slices.IndexFunc(patched.InitContainers, func(c corev1.Container) bool { return c.Name == argoInitName })
			if waiter < 0 || init <= waiter {
				t.Fatalf("measured driver lost the executor-token barrier before init: %+v", patched.InitContainers)
			}
			barrier := patched.InitContainers[waiter]
			if barrier.Image != config.SourceSeedImage || !slices.Equal(barrier.Command, []string{"sh", "-c", executorTokenWaitCommand}) || !slices.Contains(barrier.VolumeMounts, corev1.VolumeMount{Name: executorTokenVolume, MountPath: executorTokenDir}) {
				t.Fatalf("measured driver's executor-token barrier changed: %+v", barrier)
			}
			measured++
		}
	})
	if creates != 4 || deletes != 4 || measured != 1 {
		t.Fatalf("creates=%d deletes=%d measured templates=%d", creates, deletes, measured)
	}
	identity, err := specIdentity(wf)
	if err != nil || wf.Annotations[identityAnnotation] != identity {
		t.Fatalf("combined transformations were not sealed: %v", err)
	}
}
