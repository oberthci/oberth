package argoworkflow

import (
	"slices"
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

func TestCleanupAbsenceGetAdmission(t *testing.T) {
	metadata := admittedVMIManifest[:strings.Index(admittedVMIManifest, "spec:\n")]
	read := &wfv1.ResourceTemplate{Action: "get", SetOwnerReference: true, Manifest: metadata, Flags: []string{"--ignore-not-found"}}
	if err := admitVMIResource("absence", read, nil); err != nil {
		t.Fatal(err)
	}
	for _, flags := range [][]string{{"--ignore-not-found=true"}, {"--ignore-not-found", "--ignore-not-found"}, {"--ignore-not-found", "--all-namespaces"}, {"--ignore-not-found", "--raw=/api/v1/secrets"}, {"--ignore-not-found", "--selector=x"}} {
		bad := read.DeepCopy()
		bad.Flags = flags
		if err := admitVMIResource("absence", bad, nil); err == nil {
			t.Fatalf("admitted flags %v", flags)
		}
	}
	for _, action := range []string{"create", "delete"} {
		bad := read.DeepCopy()
		bad.Action = action
		if err := admitVMIResource("absence", bad, nil); err == nil {
			t.Fatalf("admitted absence flag on %s", action)
		}
	}
	deadline := int64(60)
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "absence", ActiveDeadlineSeconds: &deadline, Templates: []wfv1.Template{{Name: "absence", Resource: read}}}}
	identity := testBranchIdentity()
	identity.VMLauncherServiceAccountName = "oberth-argo-vm-launcher"
	if err := Prepare(wf, Policy{KubeVirt: true}, identity); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(wf.Spec.Templates[0].Resource.Flags, []string{"--ignore-not-found"}) {
		t.Fatal("absence flag was lost during projection")
	}
}

func TestCleanupDeleteAdmission(t *testing.T) {
	metadata := admittedVMIManifest[:strings.Index(admittedVMIManifest, "spec:\n")]
	for _, kind := range []struct{ api, kind string }{{"kubevirt.io/v1", "VirtualMachineInstance"}, {"cdi.kubevirt.io/v1beta1", "DataVolume"}, {"v1", "PersistentVolumeClaim"}} {
		for _, uid := range []string{"01234567-abcd-1234-abcd-0123456789ab", "{{inputs.parameters.uid}}"} {
			manifest := strings.ReplaceAll(strings.ReplaceAll(metadata, "kubevirt.io/v1", kind.api), "VirtualMachineInstance", kind.kind)
			manifest = strings.Replace(manifest, "metadata:\n", "metadata:\n  uid: \""+uid+"\"\n", 1)
			t.Run(kind.kind+uid, func(t *testing.T) {
				candidate := &wfv1.ResourceTemplate{Action: "delete", SetOwnerReference: true, Manifest: manifest}
				if err := admitVMIResource("delete", candidate, nil); err != nil {
					t.Fatal(err)
				}
				for _, mutate := range []func(*wfv1.ResourceTemplate){
					func(r *wfv1.ResourceTemplate) { r.Action = "create" },
					func(r *wfv1.ResourceTemplate) { r.Action = "get" },
					func(r *wfv1.ResourceTemplate) { r.SetOwnerReference = false },
					func(r *wfv1.ResourceTemplate) { r.Flags = []string{"--force"} },
					func(r *wfv1.ResourceTemplate) {
						r.Manifest = strings.Replace(r.Manifest, "{{workflow.name}}-amd64", "foreign", 1)
					},
					func(r *wfv1.ResourceTemplate) {
						r.Manifest = strings.Replace(r.Manifest, uid, "{{tasks.create.outputs.parameters.uid}}", 1)
					},
					func(r *wfv1.ResourceTemplate) {
						r.Manifest = strings.Replace(r.Manifest, uid, "{{=inputs.parameters.uid}}", 1)
					},
					func(r *wfv1.ResourceTemplate) {
						r.Manifest = strings.Replace(r.Manifest, uid, "x{{inputs.parameters.uid}}", 1)
					},
					func(r *wfv1.ResourceTemplate) { r.Manifest = strings.Replace(r.Manifest, uid, "bad uid", 1) },
				} {
					bad := *candidate
					mutate(&bad)
					if err := admitVMIResource("delete", &bad, []string{"quay.io/example/"}); err == nil {
						t.Fatalf("admitted unsafe delete: %+v", bad)
					}
				}
			})
		}
	}
	for _, kind := range []string{"DataVolume", "PersistentVolumeClaim"} {
		api := "v1"
		if kind == "DataVolume" {
			api = "cdi.kubevirt.io/v1beta1"
		}
		manifest := strings.ReplaceAll(strings.ReplaceAll(metadata, "kubevirt.io/v1", api), "VirtualMachineInstance", kind)
		if err := admitVMIResource("delete", &wfv1.ResourceTemplate{Action: "delete", SetOwnerReference: true, Manifest: manifest}, nil); err == nil {
			t.Fatal("disk delete admitted without UID")
		}
	}
}

func TestCleanupVerdictAdmission(t *testing.T) {
	deadline := int64(60)
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "vm", ActiveDeadlineSeconds: &deadline, Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}}}}}
	wf.Annotations = map[string]string{CleanupVerdictAnnotation: "2"}
	if timeout, err := CleanupVerdictTimeout(wf); err != nil || timeout != 2*time.Second {
		t.Fatalf("timeout=%v err=%v", timeout, err)
	}
	policy := Policy{KubeVirt: true, ContainerDiskPrefixes: []string{"quay.io/example/"}}
	if err := Admit(wf, policy); err != nil {
		t.Fatal(err)
	}
	policy.KubeVirt = false
	if err := Admit(wf, policy); err == nil {
		t.Fatal("cleanup verdict admitted without grant")
	}
	for _, value := range []string{"", "true", "0", "301", "-1", "1s", "01", " 2"} {
		wf.Annotations[CleanupVerdictAnnotation] = value
		if _, err := CleanupVerdictTimeout(wf); err == nil {
			t.Fatalf("admitted timeout %q", value)
		}
	}
}

func TestCleanupDiskDeletesCountAgainstTemplateCap(t *testing.T) {
	deadline := int64(60)
	wf := &wfv1.Workflow{Spec: wfv1.WorkflowSpec{Entrypoint: "vm", ActiveDeadlineSeconds: &deadline, Templates: []wfv1.Template{{Name: "vm", Resource: &wfv1.ResourceTemplate{Action: "create", SetOwnerReference: true, Manifest: admittedVMIManifest}}}}}
	metadata := admittedVMIManifest[:strings.Index(admittedVMIManifest, "spec:\n")]
	metadata = strings.Replace(metadata, "metadata:\n", "metadata:\n  uid: 'disk-uid'\n", 1)
	for _, disk := range []struct{ api, kind, name string }{{"cdi.kubevirt.io/v1beta1", "DataVolume", "dv"}, {"v1", "PersistentVolumeClaim", "pvc"}} {
		manifest := strings.ReplaceAll(strings.ReplaceAll(metadata, "kubevirt.io/v1", disk.api), "VirtualMachineInstance", disk.kind)
		wf.Spec.Templates = append(wf.Spec.Templates, wfv1.Template{Name: disk.name, Resource: &wfv1.ResourceTemplate{Action: "delete", SetOwnerReference: true, Manifest: manifest}})
	}
	budget := DefaultVMIBudget()
	budget.MaxVMs = 1
	policy := Policy{KubeVirt: true, VMIBudget: budget, ContainerDiskPrefixes: []string{"quay.io/example/"}}
	if err := Admit(wf, policy); err != nil {
		t.Fatalf("three total resource templates should fit: %v", err)
	}
	extra := *wf.Spec.Templates[2].DeepCopy()
	extra.Name = "extra-pvc"
	wf.Spec.Templates = append(wf.Spec.Templates, extra)
	if err := Admit(wf, policy); err == nil || !strings.Contains(err.Error(), "4 VMI resource templates, maximum is 3") {
		t.Fatalf("disk delete bypassed cap: %v", err)
	}
}
