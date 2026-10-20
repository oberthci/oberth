package argoworkflow

import (
	"fmt"
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
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
		{name: "cpu request above cores", old: "cpu: \"2\"", replacement: "cpu: \"3\"", want: "must not exceed spec.domain.cpu.cores"},
		{name: "firmware bootloader", old: "    devices:\n", replacement: "    firmware:\n      bootloader:\n        efi: {}\n    devices:\n", want: "bootloader"},
		{name: "firmware without kernelBoot", old: "    devices:\n", replacement: "    firmware: {}\n    devices:\n", want: "admits only kernelBoot"},
		{name: "cross run name", old: "{{workflow.name}}-amd64", replacement: "other-run-vmi", want: "substitutions"},
		{name: "bad metadata name", old: "{{workflow.name}}-amd64", replacement: "{{workflow.name}}-Bad_Name", want: "metadata.name"},
		{name: "bad label", old: "oberth.ci/workflow: \"{{workflow.name}}\"", replacement: "oberth.ci/workflow: stolen", want: "substitutions"},
		{name: "extra label", old: "oberth.ci/workflow: \"{{workflow.name}}\"", replacement: "oberth.ci/workflow: \"{{workflow.name}}\"\n    other: value", want: "metadata.labels"},
		{name: "missing masquerade", old: "masquerade: {}", replacement: "bridge: {}", want: "bridge"},
		{name: "cloud init secret ref", old: "cloudInitNoCloud:\n", replacement: "cloudInitNoCloud:\n        secretRef:\n          name: stolen\n", want: "secretRef"},
		{name: "old API", old: "kubevirt.io/v1", replacement: "kubevirt.io/v1alpha3", want: "kubevirt.io/v1 VirtualMachineInstance"},
		{name: "second YAML document", change: func(r *wfv1.ResourceTemplate) { r.Manifest += "---\napiVersion: v1\nkind: Secret\n" }, want: "one YAML document"},
		{name: "CRLF document marker", change: func(r *wfv1.ResourceTemplate) { r.Manifest += "---\r\napiVersion: v1\r\nkind: Secret\r\n" }, want: "carriage returns"},
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

func TestVMILauncherIdentityIsTemplateScoped(t *testing.T) {
	deadline := int64(3600)
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{
		Entrypoint: "vm", ActiveDeadlineSeconds: &deadline,
		Templates: []wfv1.Template{
			{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}, Executor: &wfv1.ExecutorConfig{ServiceAccountName: "attacker"}},
			{Name: "driver", Container: &corev1.Container{Image: "busybox:1.37@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, Executor: &wfv1.ExecutorConfig{ServiceAccountName: "attacker"}},
		},
	}}
	identity := testBranchIdentity()
	identity.VMLauncherServiceAccountName = "oberth-argo-vm-launcher"
	policy := Policy{KubeVirt: true, RunnerImagePrefixes: []string{"busybox:"}, ContainerDiskPrefixes: []string{"quay.io/example/"}}
	if err := Prepare(wf, policy, identity); err != nil {
		t.Fatal(err)
	}
	if got := wf.Spec.Templates[0].Executor.ServiceAccountName; got != identity.VMLauncherServiceAccountName {
		t.Fatalf("VM executor = %q", got)
	}
	if got := wf.Spec.Templates[1].Executor.ServiceAccountName; got != identity.ExecutorServiceAccountName {
		t.Fatalf("driver executor = %q", got)
	}
	if got := wf.Spec.Executor.ServiceAccountName; got != identity.ExecutorServiceAccountName {
		t.Fatalf("workflow executor = %q", got)
	}
	identity.VMLauncherServiceAccountName = ""
	if err := ForceIdentity(wf, identity); err == nil || !strings.Contains(err.Error(), "requires a server-assigned VM-launcher") {
		t.Fatalf("missing launcher identity: %v", err)
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
		workflow.Spec.Templates[i].Resource.Manifest = strings.Replace(workflow.Spec.Templates[i].Resource.Manifest, "cpu: \"4\"", "cpu: \"3\"", 1)
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

// p2pVMIWorkflow is the #919 p2p-3 topology: three 4-core, 8Gi VMIs, each
// with its create, get and delete template.
func p2pVMIWorkflow() *wfv1.Workflow {
	deadline := int64(3600)
	workflow := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "create-0", ActiveDeadlineSeconds: &deadline}}
	for i := 0; i < 3; i++ {
		manifest := strings.Replace(admittedVMIManifest, "{{workflow.name}}-amd64", fmt.Sprintf("{{workflow.name}}-p2p%d", i), 1)
		manifest = strings.Replace(manifest, "cores: 2", "cores: 4", 1)
		manifest = strings.Replace(manifest, "cpu: \"2\"", "cpu: \"4\"", 1)
		manifest = strings.Replace(manifest, "memory: 2Gi", "memory: 8Gi", 1)
		metadata := manifest[:strings.Index(manifest, "spec:\n")]
		workflow.Spec.Templates = append(workflow.Spec.Templates,
			wfv1.Template{Name: fmt.Sprintf("create-%d", i), Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: manifest}},
			wfv1.Template{Name: fmt.Sprintf("get-%d", i), Resource: &wfv1.ResourceTemplate{Action: "get", SetOwnerReference: true, Manifest: metadata}},
			wfv1.Template{Name: fmt.Sprintf("delete-%d", i), Resource: &wfv1.ResourceTemplate{Action: "delete", SetOwnerReference: true, Manifest: metadata}},
		)
	}
	return workflow
}

func TestVMIGrantBudget(t *testing.T) {
	prefixes := []string{"quay.io/example/"}
	p2p := VMIBudget{MaxVMs: 3, MaxCores: 12, MaxMemory: resource.MustParse("24Gi")}

	err := Admit(p2pVMIWorkflow(), Policy{KubeVirt: true, ContainerDiskPrefixes: prefixes})
	if err == nil || !strings.Contains(err.Error(), "3 create templates (maximum 2), 12 cores (maximum 6), 24Gi memory (maximum 12Gi)") ||
		!strings.Contains(err.Error(), "9 VMI resource templates, maximum is 6") {
		t.Fatalf("p2p-3 under the default budget: want refusal, got %v", err)
	}
	if err := Admit(p2pVMIWorkflow(), Policy{KubeVirt: true, ContainerDiskPrefixes: prefixes, VMIBudget: p2p}); err != nil {
		t.Fatalf("p2p-3 under a 3 VM / 12 core / 24Gi grant refused: %v", err)
	}
	if err := Admit(p2pVMIWorkflow(), Policy{KubeVirt: true, ContainerDiskPrefixes: prefixes, VMIBudget: p2p, VMICeiling: VMIBudgetLimit()}); err != nil {
		t.Fatalf("p2p-3 under the grant and the largest ceiling refused: %v", err)
	}

	for _, tc := range []struct {
		name           string
		grant, ceiling VMIBudget
		want           string
	}{
		{"grant cores", VMIBudget{MaxVMs: 3, MaxCores: 11, MaxMemory: resource.MustParse("24Gi")}, VMIBudget{}, "12 cores (maximum 11)"},
		{"grant memory", VMIBudget{MaxVMs: 3, MaxCores: 12, MaxMemory: resource.MustParse("23Gi")}, VMIBudget{}, "24Gi memory (maximum 23Gi)"},
		{"ceiling VMs", p2p, VMIBudget{MaxVMs: 2, MaxCores: 64, MaxMemory: resource.MustParse("256Gi")}, "3 create templates (maximum 2)"},
		{"ceiling cores", p2p, VMIBudget{MaxVMs: 16, MaxCores: 8, MaxMemory: resource.MustParse("256Gi")}, "cluster ceiling of 16 VMs, 8 cores, 256Gi memory"},
		{"invalid grant", VMIBudget{MaxVMs: 17, MaxCores: 12, MaxMemory: resource.MustParse("24Gi")}, VMIBudget{}, "KubeVirt grant: VMI budget max VMs must be 1..16"},
		{"partial grant", VMIBudget{MaxVMs: 3}, VMIBudget{}, "max cores must be 1..64"},
		{"invalid ceiling", p2p, VMIBudget{MaxVMs: 3, MaxCores: 12, MaxMemory: resource.MustParse("300Gi")}, "KubeVirt ceiling: VMI budget max memory must be 1Gi..256Gi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := Admit(p2pVMIWorkflow(), Policy{KubeVirt: true, ContainerDiskPrefixes: prefixes, VMIBudget: tc.grant, VMICeiling: tc.ceiling})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}

const kernelBootFirmware = `    firmware:
      kernelBoot:
        kernelArgs: console=ttyS0 root=/dev/vda1 ro lsm=landlock,lockdown,yama,integrity,apparmor,bpf
        container:
          image: quay.io/example/kernel-6.12@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
          kernelPath: /boot/vmlinuz
          initrdPath: /boot/initrd.img
`

func TestVMIKernelBootAdmission(t *testing.T) {
	prefixes := []string{"quay.io/example/"}
	manifest := strings.Replace(admittedVMIManifest, "    devices:\n", kernelBootFirmware+"    devices:\n", 1)
	base := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: manifest}
	if err := admitVMIResource("resource", base, prefixes); err != nil {
		t.Fatalf("pinned allowlisted kernelBoot refused: %v", err)
	}
	withoutInitrd := strings.Replace(manifest, "          initrdPath: /boot/initrd.img\n", "", 1)
	if err := admitVMIResource("resource", &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: withoutInitrd}, prefixes); err != nil {
		t.Fatalf("kernelBoot without initrd refused: %v", err)
	}
	const image = "quay.io/example/kernel-6.12@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct{ name, old, replacement, want string }{
		{"unpinned image", image, "quay.io/example/kernel-6.12:v1", "@sha256:<64 hex> digest"},
		{"latest tag", image, "quay.io/example/kernel-6.12:latest", "digest"},
		{"off-list prefix", image, "quay.io/other/kernel-6.12@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "allowlisted containerDisk prefix"},
		{"prefix boundary", image, "quay.io/examples-evil/kernel@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", "allowlisted containerDisk prefix"},
		{"image substitution", "kernel-6.12@", "{{inputs.parameters.kernel}}@", "substitutions"},
		{"pull secret", "          kernelPath:", "          imagePullSecret: stolen\n          kernelPath:", "imagePullSecret"},
		{"pull policy", "          kernelPath:", "          imagePullPolicy: Always\n          kernelPath:", "imagePullPolicy"},
		{"missing container", "        container:\n", "        other:\n", "other"},
		{"missing kernel path", "          kernelPath: /boot/vmlinuz\n", "", "kernelPath is required"},
		{"relative kernel path", "kernelPath: /boot/vmlinuz", "kernelPath: boot/vmlinuz", "kernelPath must be an absolute path"},
		{"dotdot kernel path", "kernelPath: /boot/vmlinuz", "kernelPath: /boot/../etc/shadow", "kernelPath must be an absolute path"},
		{"dot kernel path", "kernelPath: /boot/vmlinuz", "kernelPath: /boot/./vmlinuz", "kernelPath must be an absolute path"},
		{"dotdot initrd path", "initrdPath: /boot/initrd.img", "initrdPath: /../initrd.img", "initrdPath must be an absolute path"},
		{"double slash initrd", "initrdPath: /boot/initrd.img", "initrdPath: /boot//initrd.img", "initrdPath must be an absolute path"},
		{"trailing slash", "kernelPath: /boot/vmlinuz", "kernelPath: /boot/", "kernelPath must be an absolute path"},
		{"long path", "kernelPath: /boot/vmlinuz", "kernelPath: /" + strings.Repeat("a", 256), "at most 256 bytes"},
		{"args quote", "console=ttyS0 ", "console=ttyS0 init=\"/bin/sh\" ", "kernelArgs must be"},
		{"args brace", "console=ttyS0 ", "console=ttyS0 x={{workflow.name}} ", "substitutions"},
		{"args semicolon", "console=ttyS0 ", "console=ttyS0;reboot ", "kernelArgs must be"},
		{"args double space", "console=ttyS0 ", "console=ttyS0  ", "kernelArgs must be"},
		{"args too long", "console=ttyS0 ", "console=ttyS0 " + strings.Repeat("a", 512) + " ", "at most 512 bytes"},
		{"bootloader alongside", "      kernelBoot:\n", "      bootloader:\n        bios: {}\n      kernelBoot:\n", "bootloader"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(manifest, tc.old) {
				t.Fatalf("fixture lacks %q", tc.old)
			}
			candidate := &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: strings.Replace(manifest, tc.old, tc.replacement, 1)}
			if err := admitVMIResource("resource", candidate, prefixes); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want refusal containing %q, got %v", tc.want, err)
			}
		})
	}
}
