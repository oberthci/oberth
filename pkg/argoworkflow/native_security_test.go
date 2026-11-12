package argoworkflow

import (
	"testing"

	wfv1 "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

func TestNativePlacementNeverBypassesSecurityDenials(t *testing.T) {
	cases := []struct {
		name   string
		change func(*wfv1.Workflow)
	}{
		{"reserved-executor-env", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "ARGO_DEBUG_PAUSE_AFTER", Value: "true"}}
		}},
		{"privileged", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{Privileged: ptr.To(true)}
		}},
		{"escalation", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{AllowPrivilegeEscalation: ptr.To(true)}
		}},
		{"capabilities", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.SecurityContext = &corev1.SecurityContext{Capabilities: &corev1.Capabilities{Add: []corev1.Capability{"SYS_ADMIN"}}}
		}},
		{"host-network", func(w *wfv1.Workflow) { w.Spec.HostNetwork = ptr.To(true) }},
		{"host-pid", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"hostPID":true}` }},
		{"host-ipc", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"hostIPC":true}` }},
		{"host-path", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/"}}}}
		}},
		{"host-port", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Ports = []corev1.ContainerPort{{ContainerPort: 80, HostPort: 80}}
		}},
		{"service-account-override", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"serviceAccountName":"cluster-admin"}` }},
		{"automount-override", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"automountServiceAccountToken":true}` }},
		{"token-projection", func(w *wfv1.Workflow) {
			w.Spec.Volumes = []corev1.Volume{{Name: "token", VolumeSource: corev1.VolumeSource{Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{Path: "token"}}}}}}}
		}},
		{"secret", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Env = []corev1.EnvVar{{Name: "SECRET", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: "other-tenant"}, Key: "key"}}}}
		}},
		{"config-map", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.EnvFrom = []corev1.EnvFromSource{{ConfigMapRef: &corev1.ConfigMapEnvSource{LocalObjectReference: corev1.LocalObjectReference{Name: "other-tenant"}}}}
		}},
		{"foreign-template", func(w *wfv1.Workflow) {
			w.Spec.WorkflowTemplateRef = &wfv1.WorkflowTemplateRef{Name: "other", ClusterScope: true}
		}},
		{"foreign-namespace", func(w *wfv1.Workflow) { w.Namespace = "another-namespace" }},
		{"registry", func(w *wfv1.Workflow) { w.Spec.Templates[0].Container.Image = "untrusted.invalid/runner:latest" }},
		{"runtime-class", func(w *wfv1.Workflow) { w.Spec.PodSpecPatch = `{"runtimeClassName":"unapproved"}` }},
		{"device", func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Resources.Requests = corev1.ResourceList{"devices.example/device": resource.MustParse("1")}
		}},
		{"priority-class", func(w *wfv1.Workflow) { w.Spec.PodPriorityClassName = "system-node-critical" }},
		{"template-patch", func(w *wfv1.Workflow) { w.Spec.Templates[0].PodSpecPatch = `{"hostNetwork":true}` }},
		{"pod-ownership-label", func(w *wfv1.Workflow) {
			w.Spec.PodMetadata = &wfv1.Metadata{Labels: map[string]string{"oberth.ci/run": "other-run"}}
		}},
		{"pod-ownership-annotation", func(w *wfv1.Workflow) {
			w.Spec.PodMetadata = &wfv1.Metadata{Annotations: map[string]string{"oberth.ci/identity": "other-run"}}
		}},
		{"owner-reference", func(w *wfv1.Workflow) {
			w.OwnerReferences = []metav1.OwnerReference{{Name: "another", Kind: "Workflow", APIVersion: "argoproj.io/v1alpha1", UID: "another"}}
		}},
	}
	for _, placement := range []string{"none", "workflow", "template", "both"} {
		base := decodeAdmissible(t, nil)
		a := &corev1.Affinity{PodAntiAffinity: &corev1.PodAntiAffinity{RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{NamespaceSelector: &metav1.LabelSelector{}, Namespaces: []string{"other"}, LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"any.example/key": "value"}}, TopologyKey: "custom.example/rack", MismatchLabelKeys: []string{"tenant"}}}}}
		if placement == "workflow" || placement == "both" {
			base.Spec.Affinity = a.DeepCopy()
			base.Spec.NodeSelector = map[string]string{"arbitrary.example/key": "value"}
			base.Spec.Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		}
		if placement == "template" || placement == "both" {
			base.Spec.Templates[0].Affinity = a.DeepCopy()
			base.Spec.Templates[0].NodeSelector = map[string]string{"arbitrary.example/key": "value"}
			base.Spec.Templates[0].Tolerations = []corev1.Toleration{{Operator: corev1.TolerationOpExists}}
		}
		if err := Admit(base, Policy{}); err != nil {
			t.Fatalf("native %s placement refused: %v", placement, err)
		}
		for _, tc := range cases {
			t.Run(placement+"/"+tc.name, func(t *testing.T) {
				w := base.DeepCopy()
				tc.change(w)
				if err := Admit(w, Policy{}); err == nil {
					t.Fatal("security denial bypassed")
				}
			})
		}
	}
}

func TestNativePolicyHasNoComputeParallelismOrRetryCeiling(t *testing.T) {
	for _, change := range []func(*wfv1.Workflow){
		func(w *wfv1.Workflow) { w.Spec.ActiveDeadlineSeconds = nil },
		func(w *wfv1.Workflow) {
			w.Spec.Suspend = ptr.To(true)
			w.Spec.Templates[0].Suspend = &wfv1.SuspendTemplate{}
		},
		func(w *wfv1.Workflow) {
			w.Spec.Parallelism = ptr.To(int64(1000))
			w.Spec.Templates[0].Parallelism = ptr.To(int64(1000))
		},
		func(w *wfv1.Workflow) { w.Spec.RetryStrategy = &wfv1.RetryStrategy{} },
		func(w *wfv1.Workflow) {
			w.Spec.Templates[0].Container.Resources.Requests = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1000"), corev1.ResourceMemory: resource.MustParse("1Ti"), corev1.ResourceEphemeralStorage: resource.MustParse("100Ti")}
		},
	} {
		w := decodeAdmissible(t, nil)
		change(w)
		if err := Admit(w, Policy{}); err != nil {
			t.Fatal(err)
		}
	}
	w := decodeAdmissible(t, nil)
	w.Spec.ActiveDeadlineSeconds = ptr.To(int64(0))
	if err := Admit(w, Policy{}); err == nil {
		t.Fatal("invalid native zero deadline accepted")
	}
}
