package installer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestSyncLimitsUpgradeRemovesKeptObjectAndPreservesOtherObjects(t *testing.T) {
	const ns = "oberth-argo"
	for _, profile := range []bool{false, true} {
		t.Run(map[bool]string{false: "stock", true: "nonroot"}[profile], func(t *testing.T) {
			retired := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": retiredSyncLimitsName, "namespace": ns}, "data": map[string]any{"heavy": "2"}}
			unrelated := map[string]any{"apiVersion": "v1", "kind": "ConfigMap", "metadata": map[string]any{"name": "operator-extra", "namespace": ns}, "data": map[string]any{"key": "value"}}
			objects := []any{retired, unrelated}
			if profile {
				objects = append(nonrootProfileObjects(ns, baseNonrootProfile()), objects...)
			}
			stored := map[string]any{"extraObjects": objects}
			raw, _ := json.Marshal(stored)
			kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: ns, Annotations: map[string]string{"meta.helm.sh/release-name": "argo-workflows", "meta.helm.sh/release-namespace": ns, "helm.sh/resource-policy": "keep"}}})
			applied := false
			deps := Deps{Output: io.Discard, KubeClient: kube, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return raw, nil
				case "upgrade":
					applied = true
					// Stock upgrades filter stored extraObjects; profile upgrades use their complete values file.
					if profile {
						i := slices.Index(args, "--values")
						if i < 0 {
							t.Fatal("profile missing")
						}
						body, err := os.ReadFile(args[i+1])
						if err != nil {
							t.Fatal(err)
						}
						var values map[string]any
						if err = json.Unmarshal(body, &values); err != nil {
							t.Fatal(err)
						}
						objects := values["extraObjects"].([]any)
						if len(objects) != 2 || !reflect.DeepEqual(objects[1], unrelated) || !isNonrootProfileObject(objects[0], ns) {
							t.Fatal("profile or unrelated object lost", objects)
						}
						for _, arg := range args {
							if strings.HasPrefix(arg, "extraObjects=") {
								t.Fatal("stored list overrides newly rendered profile")
							}
						}
					}
					if !profile {
						found := false
						for _, arg := range args {
							if strings.HasPrefix(arg, "extraObjects=") {
								found = true
								var result []any
								if err := json.Unmarshal([]byte(strings.TrimPrefix(arg, "extraObjects=")), &result); err != nil {
									t.Fatal(err)
								}
								if !reflect.DeepEqual(result, []any{unrelated}) {
									t.Fatal(result)
								}
							}
						}
						if !found {
							t.Fatal("retired stored values not cleared")
						}
					}
				}
				return nil, nil
			}}
			cfg := Config{ArgoNamespace: ns}
			if profile {
				cfg.ArgoControllerProfile = argoworkflow.NonrootStaticProfile
				cfg.ArgoValuesReviewed = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			}
			if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
				t.Fatal(err)
			}
			if !applied {
				t.Fatal("upgrade was skipped")
			}
			if _, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), retiredSyncLimitsName, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
				t.Fatal("kept object survived", err)
			}
		})
	}
}
func TestSyncLimitsCleanupKeepsOperatorReplacement(t *testing.T) {
	kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: DefaultArgoNamespace}})
	if err := deleteRetiredSyncLimits(t.Context(), Config{}, Deps{KubeClient: kube, Output: io.Discard}, DefaultArgoNamespace); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if slices.Contains([]string{"delete", "patch", "update"}, action.GetVerb()) {
			t.Fatal(action)
		}
	}
}
