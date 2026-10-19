package argojob

import (
	"encoding/json"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"

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
