package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

const admittedVMIManifest = `apiVersion: kubevirt.io/v1
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
        userData: |
          #cloud-config
          runcmd:
            - echo ready
`

func TestVMIResourceAdmission(t *testing.T) {
	base := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}
	if err := admitVMIResource("spec.templates[vm].resource", base, []string{"quay.io/example/"}); err != nil {
		t.Fatalf("safe VMI refused: %v", err)
	}
	for name, alter := range map[string]func(*wfv1.ResourceTemplate){
		"action": func(r *wfv1.ResourceTemplate) { r.Action = "apply" },
		"owner":  func(r *wfv1.ResourceTemplate) { r.SetOwnerReference = false },
		"secret volume": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "containerDisk:", "secret:\n        secretName: stolen\n      containerDisk:", 1)
		},
		"host device": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "devices:\n", "devices:\n      hostDevices:\n        - name: host\n", 1)
		},
		"untrusted image": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "quay.io/example/", "quay.io/other/", 1)
		},
		"unbounded cpu": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "cores: 2", "cores: 32", 1)
		},
		"cross run name": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "{{workflow.name}}-amd64", "other-run-vmi", 1)
		},
		"missing masquerade": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "masquerade: {}", "bridge: {}", 1)
		},
		"cloud init secret ref": func(r *wfv1.ResourceTemplate) {
			r.Manifest = strings.Replace(r.Manifest, "cloudInitNoCloud:\n", "cloudInitNoCloud:\n        secretRef:\n          name: stolen\n", 1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := *base
			alter(&candidate)
			if err := admitVMIResource("spec.templates[vm].resource", &candidate, []string{"quay.io/example/"}); err == nil {
				t.Fatal("unsafe VMI admitted")
			}
		})
	}
}

func TestVMIResourceRequiresPolicyAndLocalTemplate(t *testing.T) {
	deadline := int64(3600)
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "vm", ActiveDeadlineSeconds: &deadline,
		Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{
			Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest,
		}}},
	}}
	if err := Admit(workflow, Policy{}); err == nil || !strings.Contains(err.Error(), "not admitted") {
		t.Fatalf("ungranted VMI template was not refused: %v", err)
	}
	policy := Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}}
	if err := Admit(workflow, policy); err != nil {
		t.Fatalf("granted VMI template refused: %v", err)
	}
	workflow.Spec.Templates[0].Name = "frag-deadbeef-vm"
	workflow.Spec.Entrypoint = "frag-deadbeef-vm"
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "consuming repository") {
		t.Fatalf("fragment VMI template was not refused: %v", err)
	}
}
