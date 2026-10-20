package argoworkflow

import (
	"strings"
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

func TestGuestTestsRequireAdminGrant(t *testing.T) {
	wf := kvmDocument(t)
	wf.Annotations[GuestTestTemplatesAnnotation] = "kernel-test"
	if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "admin grant") {
		t.Fatalf("ungranted guest tests: %v", err)
	}
	if err := Admit(wf, Policy{KubeVirt: true}); err != nil {
		t.Fatal(err)
	}
	// The repository cannot convert metadata into an administrator grant.
	wf.Annotations["oberth.ci/kubevirt-granted"] = "true"
	if err := Admit(wf, Policy{}); err == nil {
		t.Fatal("repository self-granted guest tests")
	}
	delete(wf.Annotations, GuestTestTemplatesAnnotation)
	if err := Admit(wf, Policy{}); err == nil || !strings.Contains(err.Error(), "admin grant") {
		t.Fatalf("removing guest annotation bypassed VM grant: %v", err)
	}
}

func TestGuestTestsPreservePodBoundary(t *testing.T) {
	for name, mutate := range map[string]func(*wfv1.Workflow){
		"missing KVM":  func(w *wfv1.Workflow) { delete(w.Annotations, KVMTemplatesAnnotation) },
		"empty":        func(w *wfv1.Workflow) { w.Annotations[GuestTestTemplatesAnnotation] = "" },
		"unknown":      func(w *wfv1.Workflow) { w.Annotations[GuestTestTemplatesAnnotation] = "other" },
		"duplicate":    func(w *wfv1.Workflow) { w.Annotations[GuestTestTemplatesAnnotation] = "kernel-test,kernel-test" },
		"host network": func(w *wfv1.Workflow) { w.Spec.HostNetwork = ptr.To(true) },
		"privileged": func(w *wfv1.Workflow) {
			w.Spec.Templates[1].Container.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		},
		"capability": func(w *wfv1.Workflow) {
			w.Spec.Templates[1].Container.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"BPF", "PERFMON", "SYS_ADMIN"}}}
		},
		"host device": func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "fuse", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/dev/fuse"}}}}
		},
		"cache": func(w *wfv1.Workflow) { w.Spec.Templates[1].Metadata.Annotations[WorkspaceMountsAnnotation] = "cache" },
		"token": func(w *wfv1.Workflow) { w.Spec.Templates[1].AutomountServiceAccountToken = ptr.To(true) },
	} {
		t.Run(name, func(t *testing.T) {
			wf := kvmDocument(t)
			wf.Annotations[GuestTestTemplatesAnnotation] = "kernel-test"
			mutate(wf)
			if err := Admit(wf, Policy{KubeVirt: true}); err == nil {
				t.Fatal("unsafe guest test admitted")
			}
		})
	}
}
