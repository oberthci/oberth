package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/periapsis"
)

const kubeVirtWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: vm
  activeDeadlineSeconds: 3600
  templates:
    - name: vm
      resource:
        action: create
        setOwnerReference: true
        manifest: |
          apiVersion: kubevirt.io/v1
          kind: VirtualMachineInstance
          metadata:
            name: "{{workflow.name}}-amd64"
            labels:
              oberth.ci/workflow: "{{workflow.name}}"
          spec:
            architecture: amd64
            domain:
              cpu:
                cores: 2
              resources:
                requests:
                  cpu: "2"
                  memory: 2Gi
              devices:
                disks:
                  - name: root
                    disk:
                      bus: virtio
                  - name: init
                    disk:
                      bus: virtio
                interfaces:
                  - name: default
                    masquerade: {}
            networks:
              - name: default
                pod: {}
            volumes:
              - name: root
                containerDisk:
                  image: quay.io/example/disk@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
              - name: init
                cloudInitNoCloud:
                  userData: "#cloud-config"
`

func TestBuildKubeVirtRequiresGrantAndRefusesCredentialed(t *testing.T) {
	config := testConfig()
	config.VMLauncherServiceAccount = "oberth-argo-vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	request := testRequest(periapsis.TriggerCI, kubeVirtWorkflow)
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("ungranted VMI = %v", err)
	}
	request.KubeVirtGranted = true
	wf, err := Build(config, request)
	if err != nil {
		t.Fatal(err)
	}
	if got := wf.Spec.Templates[0].Executor.ServiceAccountName; got != config.VMLauncherServiceAccount {
		t.Fatalf("VM executor = %q", got)
	}
	if got := wf.Spec.Executor.ServiceAccountName; got != config.ExecutorServiceAccount {
		t.Fatalf("shared executor = %q", got)
	}
	credentialedSource := strings.Replace(kubeVirtWorkflow, "kind: Workflow\n", "kind: Workflow\nmetadata:\n  annotations:\n    oberth.ci/secret-paths: oberth/upstream/skipops/oberth/test-secret\n", 1)
	request.Source = []byte(credentialedSource)
	request.ApprovedSecrets = map[string]bool{"oberth/upstream/skipops/oberth/test-secret": true}
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "refused in credentialed workflows") {
		t.Fatalf("credentialed VMI = %v", err)
	}
}

// VMI quota binding, create guarding and executor identity resolution all rely
// on resource templates being top-level and named. Inline containers are legal,
// but an inline VMI create must never reach those post-admission projections.
func TestBuildRefusesInlineVMICreate(t *testing.T) {
	config := testConfig()
	config.VMLauncherServiceAccount = "oberth-argo-vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	request := testRequest(periapsis.TriggerCI, kubeVirtWorkflow)
	request.KubeVirtGranted = true
	if _, err := Build(config, request); err != nil {
		t.Fatalf("top-level named VMI control refused: %v", err)
	}
	for _, kind := range []string{"dag", "steps", "nested"} {
		for _, name := range []string{"", "inline-vm"} {
			t.Run(kind+"/name="+name, func(t *testing.T) {
				wf, err := argoworkflow.Decode([]byte(kubeVirtWorkflow))
				if err != nil {
					t.Fatal(err)
				}
				inline := wf.Spec.Templates[0].DeepCopy()
				inline.Name = name
				main := wfv1.Template{Name: "main"}
				switch kind {
				case "dag":
					main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "vm", Inline: inline}}}
				case "steps":
					main.Steps = []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "vm", Inline: inline}}}}
				case "nested":
					main.DAG = &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "phase", Inline: &wfv1.Template{
						Steps: []wfv1.ParallelSteps{{Steps: []wfv1.WorkflowStep{{Name: "vm", Inline: inline}}}},
					}}}}
				}
				wf.Spec.Entrypoint = main.Name
				wf.Spec.Templates = []wfv1.Template{main}
				source, err := json.Marshal(wf)
				if err != nil {
					t.Fatal(err)
				}
				r := request
				r.Source = source
				built, err := Build(config, r)
				if built != nil || err == nil || !strings.Contains(err.Error(), ".inline.resource must be a named template in the consuming repository") {
					t.Fatalf("inline VMI create reached projections: workflow=%v error=%v", built, err)
				}
			})
		}
	}
}

func TestFragmentSourcedVMIRefusesCredentialedWorkflow(t *testing.T) {
	vm, err := argoworkflow.Decode([]byte(kubeVirtWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	key, fragment := plainFragment(t, "transferz/vm", "v1")
	vm.Spec.Templates[0].Name = "run"
	fragment.Source, err = json.Marshal(vm)
	if err != nil {
		t.Fatal(err)
	}
	path := "oberth/upstream/skipops/oberth/test-secret"
	request := fragmentRequest(t, consumingDocument("    oberth.ci/secret-paths: "+path+"\n", key.String()), map[argoworkflow.FragmentKey]argoworkflow.Fragment{key: fragment})
	request.ApprovedSecrets = map[string]bool{path: true}
	request.KubeVirtGranted = true
	config := testConfig()
	config.VMLauncherServiceAccount = "oberth-argo-vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "refused in credentialed workflows") {
		t.Fatalf("fragment VMI credentialed refusal = %v", err)
	}
}

func TestWIFLeafRefusesVMIWorkflow(t *testing.T) {
	vm, err := argoworkflow.Decode([]byte(kubeVirtWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	vm.Spec.Templates = append(vm.Spec.Templates, wfv1.Template{Name: "main", Container: &corev1.Container{Image: fragmentImage, Command: []string{"/bin/true"}}, Metadata: wfv1.Metadata{Annotations: map[string]string{argoworkflow.ReleaseWIFRoleAnnotation: "image-writer"}}})
	source, err := json.Marshal(vm)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest(periapsis.TriggerRelease, string(source))
	request.UpstreamName = "github"
	request.KubeVirtGranted = true
	config := wifTestConfig()
	config.VMLauncherServiceAccount = "oberth-argo-vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "refused in credentialed workflows") {
		t.Fatalf("WIF VMI credentialed refusal = %v", err)
	}
}

// The grant's budget reaches admission through the Request, and the
// server's ceiling through Config (#1017).
func TestBuildKubeVirtAppliesGrantBudgetUnderCeiling(t *testing.T) {
	config := testConfig()
	config.VMLauncherServiceAccount = "oberth-argo-vm-launcher"
	config.ContainerDiskPrefixes = []string{"quay.io/example/"}
	threeVMs := kubeVirtWorkflow
	for _, suffix := range []string{"two", "three"} {
		second := kubeVirtWorkflow[strings.Index(kubeVirtWorkflow, "    - name: vm\n"):]
		second = strings.Replace(second, "    - name: vm\n", "    - name: vm-"+suffix+"\n", 1)
		threeVMs += strings.Replace(second, "{{workflow.name}}-amd64", "{{workflow.name}}-"+suffix, 1)
	}
	request := testRequest(periapsis.TriggerCI, threeVMs)
	request.KubeVirtGranted = true
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "3 concurrent VMs (maximum 2)") {
		t.Fatalf("three VMIs under the default grant budget = %v", err)
	}
	request.KubeVirtBudget = argoworkflow.VMIBudget{MaxVMs: 3, MaxCores: 6, MaxMemory: resource.MustParse("6Gi")}
	if _, err := Build(config, request); err != nil {
		t.Fatalf("three VMIs under a three-VM grant: %v", err)
	}
	config.KubeVirtCeiling = argoworkflow.VMIBudget{MaxVMs: 2, MaxCores: 64, MaxMemory: resource.MustParse("256Gi")}
	if _, err := Build(config, request); err == nil || !strings.Contains(err.Error(), "under a cluster ceiling of 2 VMs") {
		t.Fatalf("three VMIs above the cluster ceiling = %v", err)
	}
	config.KubeVirtCeiling = argoworkflow.VMIBudget{MaxVMs: 99, MaxCores: 64, MaxMemory: resource.MustParse("256Gi")}
	if err := config.Validate(); err == nil || !strings.Contains(err.Error(), "KubeVirt ceiling") {
		t.Fatalf("invalid ceiling accepted: %v", err)
	}
}
