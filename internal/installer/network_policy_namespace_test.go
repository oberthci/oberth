package installer

import (
	"context"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestPolicyProbeNamespaceNeverInheritsInjectionOrAdmissionLabels(t *testing.T) {
	pipeline := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pipelines", Labels: map[string]string{
		"pod-security.kubernetes.io/enforce": "privileged", "pod-security.kubernetes.io/enforce-version": "v1.25",
		"pod-security.kubernetes.io/audit": "privileged", "pod-security.kubernetes.io/warn": "privileged",
		"istio-injection": "enabled", "istio.io/rev": "canary", "linkerd.io/inject": "enabled",
		"custom.example.test/inject": "yes", "domain": "pipeline", "kubernetes.io/metadata.name": "pipelines",
	}}}
	kube := fake.NewClientset(preflightNode(), pipeline)
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", readyPolicyModelPod)
	created := false
	kube.PrependReactor("create", "namespaces", func(action kt.Action) (bool, runtime.Object, error) {
		ns := action.(kt.CreateAction).GetObject().(*corev1.Namespace)
		want := map[string]string{policyProbeLabel: strings.TrimPrefix(ns.Name, "oberth-policy-probe-"),
			"pod-security.kubernetes.io/enforce": "restricted", "pod-security.kubernetes.io/enforce-version": "latest",
			"pod-security.kubernetes.io/audit": "restricted", "pod-security.kubernetes.io/audit-version": "latest",
			"pod-security.kubernetes.io/warn": "restricted", "pod-security.kubernetes.io/warn-version": "latest",
		}
		if !reflect.DeepEqual(ns.Labels, want) {
			t.Fatal("scratch inherited labels beyond ownership and restricted admission", ns.Labels)
		}
		created = true
		return false, nil, nil
	})
	model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "refused"}
	result := probeNetworkPolicy(context.Background(), Config{ArgoNamespace: "pipelines"}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming())
	if !created || result.Status != "enforced" || !strings.Contains(result.Evidence, "pipeline labels not copied") || !strings.Contains(result.Evidence, "namespace-label-specific exclusions") {
		t.Fatal("scope or measurement missing", result)
	}
	original, err := kube.CoreV1().Namespaces().Get(t.Context(), "pipelines", metav1.GetOptions{})
	if err != nil || !reflect.DeepEqual(original.Labels, pipeline.Labels) {
		t.Fatal("pipeline namespace changed", err)
	}
}
