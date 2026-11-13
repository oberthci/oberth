package installer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"

	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestSyncLimitsUpgradeOrphansKeptObjectAndPreservesOtherObjects(t *testing.T) {
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
			kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: ns, UID: "original-cm", ResourceVersion: "1", Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm", "operator.example/label": "kept"}, Annotations: map[string]string{"meta.helm.sh/release-name": "argo-workflows", "meta.helm.sh/release-namespace": ns, "operator.example/note": "kept"}}, Data: map[string]string{"heavy": "2", "kvm": "1"}, BinaryData: map[string][]byte{"opaque": {0, 1, 2}}})
			applied := 0
			var retained *corev1.ConfigMap
			deps := Deps{Output: io.Discard, KubeClient: kube, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return raw, nil
				case "upgrade":
					applied++
					cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), retiredSyncLimitsName, metav1.GetOptions{})
					if err != nil {
						t.Fatal(err)
					}
					if cm.Annotations["helm.sh/resource-policy"] != "keep" || cm.Annotations["meta.helm.sh/release-name"] != "" || cm.Annotations["meta.helm.sh/release-namespace"] != "" || cm.Labels["app.kubernetes.io/managed-by"] != "" {
						t.Fatal("Helm could delete or adopt semaphore limits", cm)
					}
					if applied == 1 {
						retained = cm.DeepCopy()
					} else if !reflect.DeepEqual(retained, cm) {
						t.Fatal("second upgrade changed operator object")
					}
					var remaining []any
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
						remaining = objects
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
								remaining = result
								if !reflect.DeepEqual(result, []any{unrelated}) {
									t.Fatal(result)
								}
							}
						}
						if applied == 1 && !found {
							t.Fatal("retired stored values not cleared")
						}
					}
					if !profile && remaining == nil {
						remaining = []any{unrelated}
					}
					stored["extraObjects"] = remaining
					raw, _ = json.Marshal(stored)
				}
				return nil, nil
			}}
			cfg := Config{ArgoNamespace: ns}
			if profile {
				cfg.ArgoControllerProfile = argoworkflow.NonrootStaticProfile
				cfg.ArgoValuesReviewed = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
			}
			for range 2 {
				if profile {
					cfg.ArgoValuesReviewed = fmt.Sprintf("sha256:%x", sha256.Sum256(raw))
				}
				cfg.Upgrade = true
				if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
					t.Fatal(err)
				}
			}
			if applied != 2 {
				t.Fatal("both upgrades must run", applied)
			}
			cm, err := kube.CoreV1().ConfigMaps(ns).Get(t.Context(), retiredSyncLimitsName, metav1.GetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(cm.Data, map[string]string{"heavy": "2", "kvm": "1"}) || !reflect.DeepEqual(cm.BinaryData, map[string][]byte{"opaque": {0, 1, 2}}) || cm.UID != "original-cm" || cm.Labels["operator.example/label"] != "kept" || cm.Annotations["operator.example/note"] != "kept" {
				t.Fatal("operator data or identity changed", cm)
			}
			patches := 0
			for _, a := range kube.Actions() {
				if a.GetResource().Resource == "configmaps" {
					if a.GetVerb() == "delete" {
						t.Fatal("semaphore object deleted")
					}
					if a.GetVerb() == "patch" {
						patches++
					}
				}
			}
			if patches != 1 {
				t.Fatal("second upgrade still manages orphan", patches)
			}

		})
	}
}
func TestSyncLimitsCleanupKeepsOperatorReplacement(t *testing.T) {
	kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: DefaultArgoNamespace}})
	if err := orphanRetiredSyncLimits(t.Context(), Config{}, Deps{KubeClient: kube, Output: io.Discard}, DefaultArgoNamespace); err != nil {
		t.Fatal(err)
	}
	for _, action := range kube.Actions() {
		if slices.Contains([]string{"delete", "patch", "update"}, action.GetVerb()) {
			t.Fatal(action)
		}
	}
}

func TestSyncLimitsOrphanFailurePreventsHelmUpgrade(t *testing.T) {
	ns := DefaultArgoNamespace
	kube := fake.NewClientset(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: retiredSyncLimitsName, Namespace: ns, Annotations: map[string]string{"meta.helm.sh/release-name": "argo-workflows", "meta.helm.sh/release-namespace": ns}}})
	kube.PrependReactor("patch", "configmaps", func(ktesting.Action) (bool, runtime.Object, error) { return true, nil, errors.New("write denied") })
	upgraded := false
	deps := Deps{KubeClient: kube, Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		switch args[0] {
		case "list":
			return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
		case "get":
			return []byte(`{}`), nil
		case "upgrade":
			upgraded = true
		}
		return nil, nil
	}}
	if _, err := InstallArgoWorkflows(t.Context(), Config{Upgrade: true}, deps); err == nil || !strings.Contains(err.Error(), "orphan semaphore") {
		t.Fatal("unsafe upgrade allowed", err)
	}
	if upgraded {
		t.Fatal("Helm ran after orphaning failed")
	}
}
func TestSyncLimitsFreshInstallCreatesNone(t *testing.T) {
	kube := fake.NewClientset()
	deps := Deps{KubeClient: kube, Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
		if args[0] == "list" {
			return []byte(`[]`), nil
		}
		if args[0] == "install" {
			for _, a := range args {
				if strings.Contains(a, retiredSyncLimitsName) {
					t.Fatal("fresh install renders limits", args)
				}
			}
		}
		return nil, nil
	}}
	if _, err := InstallArgoWorkflows(t.Context(), Config{}, deps); err != nil {
		t.Fatal(err)
	}
	maps, err := kube.CoreV1().ConfigMaps(DefaultArgoNamespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(maps.Items) != 0 {
		t.Fatal("fresh install created limits", maps, err)
	}
}
