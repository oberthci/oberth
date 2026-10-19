package argoworkflow

import (
	"fmt"
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
	const substitution = "{{inputs.parameters.inject}}"
	for _, tc := range []struct {
		name, old, replacement, want string
		change                       func(*wfv1.ResourceTemplate)
	}{
		{name: "action", change: func(r *wfv1.ResourceTemplate) { r.Action = "apply" }, want: ".action"},
		{name: "owner", change: func(r *wfv1.ResourceTemplate) { r.SetOwnerReference = false }, want: ".setOwnerReference"},
		{name: "secret volume", old: "containerDisk:", replacement: "secret:\n        secretName: stolen\n      containerDisk:", want: "secret"},
		{name: "host device", old: "devices:\n", replacement: "devices:\n      hostDevices:\n        - name: host\n", want: "hostDevices"},
		{name: "untrusted image", old: "quay.io/example/", replacement: "quay.io/other/", want: "containerDisk.image"},
		{name: "prefix boundary", old: "quay.io/example/", replacement: "quay.io/examples-evil/", want: "containerDisk.image"},
		{name: "unbounded cpu", old: "cores: 2", replacement: "cores: 32", want: "cpu.cores"},
		{name: "cross run name", old: "{{workflow.name}}-amd64", replacement: "other-run-vmi", want: "substitutions"},
		{name: "bad metadata name", old: "{{workflow.name}}-amd64", replacement: "{{workflow.name}}-Bad_Name", want: "metadata.name"},
		{name: "bad label", old: "oberth.ci/workflow: \"{{workflow.name}}\"", replacement: "oberth.ci/workflow: stolen", want: "substitutions"},
		{name: "extra label", old: "oberth.ci/workflow: \"{{workflow.name}}\"", replacement: "oberth.ci/workflow: \"{{workflow.name}}\"\n    other: value", want: "metadata.labels"},
		{name: "missing masquerade", old: "masquerade: {}", replacement: "bridge: {}", want: "bridge"},
		{name: "cloud init secret ref", old: "cloudInitNoCloud:\n", replacement: "cloudInitNoCloud:\n        secretRef:\n          name: stolen\n", want: "secretRef"},
		{name: "old API", old: "kubevirt.io/v1", replacement: "kubevirt.io/v1alpha3", want: "kubevirt.io/v1 VirtualMachineInstance"},
		{name: "second YAML document", change: func(r *wfv1.ResourceTemplate) { r.Manifest += "---\napiVersion: v1\nkind: Secret\n" }, want: "one YAML document"},
		{name: "metadata name substitution", old: "{{workflow.name}}-amd64", replacement: substitution, want: "substitutions"},
		{name: "run label substitution", old: "oberth.ci/workflow: \"{{workflow.name}}\"", replacement: "oberth.ci/workflow: \"" + substitution + "\"", want: "substitutions"},
		{name: "volume name substitution", old: "- name: root\n      containerDisk:", replacement: "- name: \"" + substitution + "\"\n      containerDisk:", want: "substitutions"},
		{name: "disk name substitution", old: "- name: root\n          disk:", replacement: "- name: \"" + substitution + "\"\n          disk:", want: "substitutions"},
		{name: "network name substitution", old: "- name: default\n      pod:", replacement: "- name: \"" + substitution + "\"\n      pod:", want: "substitutions"},
		{name: "interface name substitution", old: "- name: default\n          masquerade:", replacement: "- name: \"" + substitution + "\"\n          masquerade:", want: "substitutions"},
		{name: "image substitution", old: "quay.io/example/disk@", replacement: "quay.io/example/" + substitution + "@", want: "substitutions"},
		{name: "cloud init substitution", old: "echo ready", replacement: "echo " + substitution, want: "substitutions"},
		{name: "expression substitution", old: "echo ready", replacement: "echo {{=inputs.parameters.inject}}", want: "substitutions"},
		{name: "volume DNS label", old: "- name: root\n      containerDisk:", replacement: "- name: Root_Name\n      containerDisk:", want: "volumes[0].name"},
		{name: "disk DNS label", old: "- name: root\n          disk:", replacement: "- name: Root_Name\n          disk:", want: "disks[0].name"},
		{name: "network DNS label", old: "- name: default\n      pod:", replacement: "- name: Bad_Name\n      pod:", want: "DNS labels"},
		{name: "interface DNS label", old: "- name: default\n          masquerade:", replacement: "- name: Bad_Name\n          masquerade:", want: "DNS labels"},
		{name: "empty prefix list", change: func(r *wfv1.ResourceTemplate) {}, want: "no administrator containerDisk registry allowlist"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := *base
			if tc.old != "" {
				candidate.Manifest = strings.Replace(candidate.Manifest, tc.old, tc.replacement, 1)
			}
			if tc.change != nil {
				tc.change(&candidate)
			}
			prefixes := []string{"quay.io/example/"}
			if tc.name == "empty prefix list" {
				prefixes = nil
			}
			if err := admitVMIResource("spec.templates[vm].resource", &candidate, prefixes); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want rejection containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestVMIGetDeleteAndPrefixValidation(t *testing.T) {
	manifest := admittedVMIManifest[:strings.Index(admittedVMIManifest, "spec:\n")]
	for _, action := range []string{"get", "delete"} {
		t.Run(action, func(t *testing.T) {
			candidate := &wfv1.ResourceTemplate{Action: action, SetOwnerReference: true, Manifest: manifest}
			if err := admitVMIResource("resource", candidate, nil); err != nil {
				t.Fatalf("valid %s refused: %v", action, err)
			}
			candidate.Manifest += "spec:\n  domain:\n    devices:\n      disks:\n        - name: root\n"
			if err := admitVMIResource("resource", candidate, nil); err == nil || !strings.Contains(err.Error(), "manifest.spec must be absent") {
				t.Fatalf("expected spec rejection, got %v", err)
			}
		})
	}
	for _, tc := range []struct {
		prefixes []string
		want     string
	}{
		{[]string{"quay.io/example"}, "path boundary"},
		{[]string{""}, "clean nonempty"},
		{[]string{"quay.io/example/", "quay.io/example/"}, "duplicated"},
	} {
		candidate := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}
		if err := admitVMIResource("resource", candidate, tc.prefixes); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("prefixes %q: want %q, got %v", tc.prefixes, tc.want, err)
		}
	}
}

func TestVMIEmptyDiskBudget(t *testing.T) {
	manifest := admittedVMIManifest
	manifest = strings.Replace(manifest, "        - name: init\n          disk:", "        - name: scratch-a\n          disk:\n            bus: virtio\n        - name: scratch-b\n          disk:\n            bus: virtio\n        - name: init\n          disk:", 1)
	manifest += "    - name: scratch-a\n      emptyDisk:\n        capacity: 9Gi\n    - name: scratch-b\n      emptyDisk:\n        capacity: 9Gi\n"
	candidate := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: manifest}
	if err := admitVMIResource("resource", candidate, []string{"quay.io/example/"}); err == nil || !strings.Contains(err.Error(), "total emptyDisk capacity") {
		t.Fatalf("expected summed emptyDisk rejection, got %v", err)
	}
}

func TestVMIPerRunBudget(t *testing.T) {
	deadline := int64(3600)
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "vm-0", ActiveDeadlineSeconds: &deadline}}
	policy := Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}}
	for i := 0; i < 3; i++ {
		manifest := strings.Replace(admittedVMIManifest, "{{workflow.name}}-amd64", fmt.Sprintf("{{workflow.name}}-vm%d", i), 1)
		manifest = strings.Replace(manifest, "cores: 2", "cores: 4", 1)
		manifest = strings.Replace(manifest, "cpu: \"2\"", "cpu: \"4\"", 1)
		manifest = strings.Replace(manifest, "memory: 2Gi", "memory: 8Gi", 1)
		workflow.Spec.Templates = append(workflow.Spec.Templates, wfv1.Template{Name: fmt.Sprintf("vm-%d", i), Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: manifest}})
	}
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "VMI run budget exceeded") {
		t.Fatalf("expected per-run budget rejection, got %v", err)
	}
	workflow.Spec.Templates = workflow.Spec.Templates[:2]
	for i := range workflow.Spec.Templates {
		workflow.Spec.Templates[i].Resource.Manifest = strings.Replace(workflow.Spec.Templates[i].Resource.Manifest, "cores: 4", "cores: 3", 1)
		workflow.Spec.Templates[i].Resource.Manifest = strings.Replace(workflow.Spec.Templates[i].Resource.Manifest, "memory: 8Gi", "memory: 6Gi", 1)
	}
	if err := Admit(workflow, policy); err != nil {
		t.Fatalf("two VMIs within run budget refused: %v", err)
	}
	workflow.Spec.Templates[1].Resource.Manifest = strings.Replace(workflow.Spec.Templates[1].Resource.Manifest, "cores: 3", "cores: 4", 1)
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "7 cores (maximum 6)") {
		t.Fatalf("expected per-run core rejection, got %v", err)
	}
	workflow.Spec.Templates[1].Resource.Manifest = strings.Replace(workflow.Spec.Templates[1].Resource.Manifest, "cores: 4", "cores: 3", 1)
	workflow.Spec.Templates[1].Resource.Manifest = strings.Replace(workflow.Spec.Templates[1].Resource.Manifest, "memory: 6Gi", "memory: 8Gi", 1)
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "14Gi memory (maximum 12Gi)") {
		t.Fatalf("expected per-run memory rejection, got %v", err)
	}
	workflow.Spec.Templates[1].Resource.Manifest = strings.Replace(workflow.Spec.Templates[1].Resource.Manifest, "memory: 8Gi", "memory: 6Gi", 1)
	for i := 0; i < 5; i++ {
		workflow.Spec.Templates = append(workflow.Spec.Templates, wfv1.Template{Name: fmt.Sprintf("get-%d", i), Resource: &wfv1.ResourceTemplate{Action: "get", SetOwnerReference: true, Manifest: admittedVMIManifest[:strings.Index(admittedVMIManifest, "spec:\n")]}})
	}
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "VMI resource templates, maximum") {
		t.Fatalf("expected VMI template count rejection, got %v", err)
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
	workflow.Spec.TemplateDefaults = &wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}}
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "must be a named template") {
		t.Fatalf("templateDefaults resource was not refused: %v", err)
	}
	workflow.Spec.TemplateDefaults = nil
	workflow.Spec.Templates = append(workflow.Spec.Templates, wfv1.Template{Name: "driver", DAG: &wfv1.DAGTemplate{Tasks: []wfv1.DAGTask{{Name: "inline", Inline: &wfv1.Template{Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}}}}}})
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "must be a named template") {
		t.Fatalf("inline resource was not refused: %v", err)
	}
	workflow.Spec.Templates = workflow.Spec.Templates[:1]
	workflow.Spec.Templates[0].Name = "frag-deadbeef-vm"
	workflow.Spec.Entrypoint = "frag-deadbeef-vm"
	if err := Admit(workflow, policy); err == nil || !strings.Contains(err.Error(), "consuming repository") {
		t.Fatalf("fragment VMI template was not refused: %v", err)
	}
}
