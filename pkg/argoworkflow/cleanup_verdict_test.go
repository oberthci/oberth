package argoworkflow

import (
	"strings"
	"testing"
	"time"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
)

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
