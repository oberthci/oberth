package installer

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// Without limit flags the Argo install changes no limit, so --reuse-values
// keeps whatever an earlier install set.
func TestArgoHelmArgsSetNoLimitUnlessAsked(t *testing.T) {
	for _, arg := range ArgoHelmArgs(Config{}) {
		if strings.Contains(arg, "parallelism") || strings.Contains(arg, "extraObjects") {
			t.Fatalf("unrequested limit in %q", arg)
		}
	}
}

// The limits land in the argo-workflows release: controller parallelism and
// the ConfigMap oberth-sync-limits in the pipeline namespace (#1517).
func TestArgoHelmArgsRenderTheRequestedLimits(t *testing.T) {
	parallelism, namespaced := 4, 0
	args := ArgoHelmArgs(Config{ArgoParallelism: &parallelism, ArgoNamespaceParallelism: &namespaced,
		ArgoSyncLimits: map[string]int{"kvm": 1, "heavy-go": 2}})
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"--set controller.parallelism=4",
		"--set controller.namespaceParallelism=0",
		`--set-json extraObjects=[{"apiVersion":"v1","data":{"heavy-go":"2","kvm":"1"},"kind":"ConfigMap","metadata":{`,
		`"name":"oberth-sync-limits","namespace":"oberth-argo"`,
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("args lack %q:\n%s", want, joined)
		}
	}
	if !slices.Contains(ArgoHelmArgs(Config{ArgoSyncLimits: map[string]int{}}), "extraObjects=[]") {
		t.Fatal("an empty limit set must clear the ConfigMap")
	}
}

func TestParseArgoSyncLimit(t *testing.T) {
	if key, limit, err := ParseArgoSyncLimit("heavy-go=2"); err != nil || key != "heavy-go" || limit != 2 {
		t.Fatalf("parse = %q %d %v", key, limit, err)
	}
	for _, bad := range []string{"kvm", "kvm=0", "kvm=-1", "kvm=x", "-kvm=1", "=1", "a b=1"} {
		if _, _, err := ParseArgoSyncLimit(bad); err == nil {
			t.Fatalf("%q accepted", bad)
		}
	}
}

func TestValidateArgoLimitsRefusesAnUnmanagedController(t *testing.T) {
	one, negative := 1, -1
	if err := validateArgoLimits(Config{SkipArgo: true, ArgoParallelism: &one}); err == nil {
		t.Fatal("limits accepted with --skip-argo-workflows")
	}
	if err := validateArgoLimits(Config{ArgoParallelism: &negative}); err == nil {
		t.Fatal("negative parallelism accepted")
	}
}

// The ConfigMap an Oberth v0.18/v0.19 release rendered is handed to the
// argo-workflows release with resource-policy keep, so neither upgrade order
// deletes it under running workflows; an already handed-over one is left
// alone, and nothing happens unless limits are requested.
func TestHandOverSyncLimitsReownsTheOberthConfigMap(t *testing.T) {
	ctx := context.Background()
	kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: SyncLimitsConfigMap, Namespace: "oberth-argo",
		Annotations: map[string]string{helmReleaseNameAnnotation: "oberth", helmReleaseNamespaceAnnotation: "oberth"},
		Labels:      map[string]string{helmManagedByLabel: "Helm"}}, Data: map[string]string{"kvm": "1"}})
	deps := Deps{KubeClient: kube, Output: io.Discard}
	if err := handOverSyncLimits(ctx, Config{}, deps, "oberth-argo"); err != nil {
		t.Fatal(err)
	}
	current, _ := kube.CoreV1().ConfigMaps("oberth-argo").Get(ctx, SyncLimitsConfigMap, metav1.GetOptions{})
	if current.Annotations[helmReleaseNameAnnotation] != "oberth" {
		t.Fatal("handed over without a limit request")
	}
	if err := handOverSyncLimits(ctx, Config{ArgoSyncLimits: map[string]int{"kvm": 1}}, deps, "oberth-argo"); err != nil {
		t.Fatal(err)
	}
	current, _ = kube.CoreV1().ConfigMaps("oberth-argo").Get(ctx, SyncLimitsConfigMap, metav1.GetOptions{})
	if current.Annotations[helmReleaseNameAnnotation] != argoReleaseName || current.Annotations[helmReleaseNamespaceAnnotation] != "oberth-argo" ||
		current.Annotations[helmResourcePolicyAnnotation] != "keep" || current.Data["kvm"] != "1" {
		t.Fatalf("handover = %+v", current.ObjectMeta)
	}
	if err := handOverSyncLimits(ctx, Config{ArgoSyncLimits: map[string]int{"kvm": 1}}, Deps{KubeClient: fake.NewClientset(), Output: io.Discard}, "oberth-argo"); err != nil {
		t.Fatalf("absent ConfigMap: %v", err)
	}
}
