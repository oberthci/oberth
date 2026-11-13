package installer

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestIntegrationRetiringSemaphorePreservesOperatorReplacement(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		keep, noClient, allow bool
	}{
		{name: "unretained replacement"},
		{name: "explicitly retained replacement", keep: true, allow: true},
		{name: "missing client", noClient: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ns := DefaultArgoNamespace
			cmStart := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: ns, UID: "operator-replacement", ResourceVersion: "5"}, Data: map[string]string{"custom": "operator-value"}}
			if tc.keep {
				cmStart.Annotations = map[string]string{"helm.sh/resource-policy": "keep"}
			}
			kube := fake.NewClientset(cmStart)
			oldValues, _ := json.Marshal(map[string]any{"extraObjects": []any{map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": retiredSyncLimitsName, "namespace": ns}, "data": map[string]any{"old": "1"}}}})
			upgraded := false
			deps := Deps{Output: io.Discard, KubeClient: kube, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return oldValues, nil
				case "upgrade":
					upgraded = true
					// Helm4.2.3 originals.Difference(targets) refetches the live object,
					// then skips deletion only for its current resource-policy=keep.
					cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), retiredSyncLimitsName, metav1.GetOptions{})
					if err == nil && cm.Annotations["helm.sh/resource-policy"] != "keep" {
						err = kube.CoreV1().ConfigMaps(ns).Delete(t.Context(), retiredSyncLimitsName, metav1.DeleteOptions{})
					}
					return nil, err
				}
				return nil, nil
			}}
			if tc.noClient {
				deps.KubeClient = nil
			}
			_, installErr := InstallArgoWorkflows(t.Context(), Config{Upgrade: true}, deps)
			if (installErr == nil) != tc.allow || upgraded != tc.allow {
				t.Fatalf("upgrade=%t error=%v; allowed=%t", upgraded, installErr, tc.allow)
			}
			cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), retiredSyncLimitsName, metav1.GetOptions{})
			if apierrors.IsNotFound(err) {
				t.Fatal("Helm would delete the operator replacement still tracked by the prior manifest")
			}
			if err != nil || cm.Data["custom"] != "operator-value" {
				t.Fatalf("operator data changed: %v", err)
			}
			for _, action := range kube.Actions() {
				if action.GetVerb() == "patch" || action.GetVerb() == "delete" || action.GetVerb() == "update" {
					t.Fatal("operator replacement mutated", action.GetVerb())
				}
			}
		})
	}
}
