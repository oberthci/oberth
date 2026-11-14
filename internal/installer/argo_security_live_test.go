package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
)

func argoSecurityLiveObjects(namespace string, safe bool) []runtime.Object {
	config := `{}`
	if safe {
		_, review, _ := argoSecurityDefaults(nil)
		raw, _ := json.Marshal(map[string]any{"mainContainer": map[string]any{"securityContext": review[0].Planned}, "executor": map[string]any{"securityContext": review[1].Planned}})
		config = string(raw)
	}
	return []runtime.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: "workflow-controller", Namespace: namespace}, Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "workflow-controller", Command: []string{"workflow-controller"}, Args: []string{"--configmap", "operator-config"}}}}}}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "operator-config", Namespace: namespace}, Data: map[string]string{"config": config}},
	}
}

// Unrelated installer fixtures include an already-compliant live controller.
func argoSecurityTestClient(objects ...runtime.Object) *fake.Clientset {
	return fake.NewClientset(append(argoSecurityLiveObjects(DefaultArgoNamespace, true), objects...)...)
}

func setArgoSecurityLive(t *testing.T, client kubernetes.Interface, namespace string, safe bool) {
	t.Helper()
	cm := argoSecurityLiveObjects(namespace, safe)[1].(*corev1.ConfigMap)
	if _, err := client.CoreV1().ConfigMaps(namespace).Update(t.Context(), cm, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
}

func TestArgoSecurityLiveConsentAndMigration(t *testing.T) {
	for _, install := range []bool{false, true} {
		for _, mode := range []string{"compliant", "external-compliant", "missing-no-consent", "missing-flag", "missing-approved", "missing-declined", "missing-closed-input", "missing-external", "skip-compliant", "skip-missing", "old-profile", "old-profile-compliant", "unreadable", "wrong-configmap", "fresh"} {
			t.Run(map[bool]string{true: "install", false: "upgrade"}[install]+"/"+mode, func(t *testing.T) {
				safe := strings.Contains(mode, "compliant")
				existing := mode != "external-compliant" && mode != "missing-external" && mode != "fresh"
				stored := `{}`
				if strings.HasPrefix(mode, "old-profile") {
					stored = `{"controller":{"configMap":{"create":false}}}`
				}
				if safe {
					if strings.HasPrefix(mode, "old-profile") {
						stored = `{"controller":{"configMap":{"create":false},"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":3600}}}}}`
					} else {
						stored = `{"controller":{"workflowDefaults":{"spec":{"ttlStrategy":{"secondsAfterCompletion":3600}}}}}`
					}
				}
				var output bytes.Buffer
				writes := 0
				client := fake.NewClientset(argoSecurityLiveObjects(DefaultArgoNamespace, safe)...)
				deps := Deps{KubeClient: client, Output: &output, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					switch args[0] {
					case "list":
						if !existing {
							return []byte(`[]`), nil
						}
						return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
					case "get":
						if mode == "unreadable" {
							return nil, errors.New("denied: secret-sentinel")
						}
						return []byte(stored), nil
					case "repo":
						return nil, nil
					case "upgrade":
						writes++
						setArgoSecurityLive(t, client, DefaultArgoNamespace, true)
						return nil, nil
					}
					t.Fatalf("unexpected command %v", args)
					return nil, nil
				}}
				if mode == "wrong-configmap" {
					_ = client.CoreV1().ConfigMaps(DefaultArgoNamespace).Delete(t.Context(), "operator-config", metav1.DeleteOptions{})
				}
				if mode == "missing-approved" || mode == "missing-declined" || mode == "missing-closed-input" {
					deps.IsTerminal = func() bool { return true }
					deps.Input = strings.NewReader(map[string]string{"missing-approved": "yes\n", "missing-declined": "no\n"}[mode])
				}
				consent := mode == "missing-flag" || strings.HasPrefix(mode, "old-profile")
				var plan *upgradeArgoSecurity
				var err error
				if install {
					cfg := Config{SkipArgo: strings.HasPrefix(mode, "skip-") || mode == "external-compliant" || mode == "missing-external", ArgoSecurityDefaults: consent}
					err = readArgoSecurityReview(t.Context(), &cfg, deps)
					if err == nil {
						plan = &upgradeArgoSecurity{namespace: DefaultArgoNamespace, args: cfg.argoSecurityArgs, consent: cfg.ArgoSecurityDefaults}
					}
				} else {
					plan, err = planUpgradeArgoSecurity(t.Context(), deps, map[string]any{}, consent)
				}
				pass := safe || mode == "missing-flag" || mode == "missing-approved" || (mode == "fresh" && install)
				if (err == nil) != pass {
					t.Fatalf("pass=%v plan=%+v err=%v", pass, plan, err)
				}
				if err != nil {
					if strings.Contains(err.Error(), "secret-sentinel") {
						t.Fatal("subprocess output leaked into public review")
					}
					if strings.HasPrefix(mode, "old-profile") {
						for _, text := range []string{"v0.22.5 migration", "controller.configMap.create", "controller.configMap.name", "volume and mount", "extraObjects", "--reset-values", "--values <reviewed-values.yaml>"} {
							if !strings.Contains(err.Error(), text) {
								t.Fatalf("migration missing %q: %v", text, err)
							}
						}
					} else if !strings.Contains(err.Error(), `executor.securityContext.capabilities.drop=["ALL"]`) {
						t.Fatal("exact operator values absent", err)
					}
				}
				if pass && (mode == "missing-flag" || mode == "missing-approved") {
					if err := plan.reconcile(t.Context(), deps); err != nil || writes != 1 {
						t.Fatalf("writes=%d err=%v", writes, err)
					}
					if !strings.Contains(output.String(), "Argo executor.securityContext:") {
						t.Fatal("changes not reviewed")
					}
				} else if writes != 0 {
					t.Fatal("unexpected mutation", writes)
				}
				if safe && plan != nil && len(plan.args) != 0 {
					t.Fatal("compliant live controller got rewrite", plan.args)
				}
			})
		}
	}
}

func TestArgoSecurityReadsReferencedLiveConfig(t *testing.T) {
	for _, mode := range []string{"native", "split-data", "equals-argument", "default-name", "wrong-reference", "executor-missing", "unsafe-add", "unreadable"} {
		t.Run(mode, func(t *testing.T) {
			objects := argoSecurityLiveObjects(DefaultArgoNamespace, true)
			deployment, cm := objects[0].(*appsv1.Deployment), objects[1].(*corev1.ConfigMap)
			switch mode {
			case "equals-argument":
				deployment.Spec.Template.Spec.Containers[0].Args = []string{"--configmap=operator-config"}
			case "default-name":
				deployment.Spec.Template.Spec.Containers[0].Args = nil
				cm.Name = "workflow-controller-configmap"
			case "split-data":
				var config map[string]json.RawMessage
				_ = json.Unmarshal([]byte(cm.Data["config"]), &config)
				cm.Data = map[string]string{"mainContainer": string(config["mainContainer"]), "executor": string(config["executor"])}
			case "wrong-reference":
				deployment.Spec.Template.Spec.Containers[0].Args = []string{"--configmap", "other"}
			case "executor-missing":
				cm.Data["config"] = `{"mainContainer":{"securityContext":{"allowPrivilegeEscalation":false,"capabilities":{"drop":["ALL"]},"seccompProfile":{"type":"RuntimeDefault"}}}}`
			case "unsafe-add":
				cm.Data["config"] = strings.ReplaceAll(cm.Data["config"], `"drop":["ALL"]`, `"drop":["ALL"],"add":["SYS_ADMIN"]`)
			case "unreadable":
				objects = objects[:1]
			}
			err := checkLiveArgoSecurity(t.Context(), Deps{KubeClient: fake.NewClientset(objects...)}, DefaultArgoNamespace)
			pass := slices.Contains([]string{"native", "split-data", "equals-argument", "default-name"}, mode)
			if (err == nil) != pass {
				t.Fatalf("pass=%v err=%v", pass, err)
			}
		})
	}
}

func TestArgoSecurityConsentIsExplicit(t *testing.T) {
	for _, answer := range []string{"y\n", "YES\n", "\n", "no\n", "yes"} {
		err := approveArgoSecurity(Deps{Output: io.Discard, Input: strings.NewReader(answer)})
		if (err == nil) != (answer == "y\n" || answer == "YES\n") {
			t.Fatal(answer, err)
		}
	}
}

func TestArgoSecurityInstallReviewUsesLiveExternalConfig(t *testing.T) {
	for _, skip := range []bool{false, true} {
		for _, safe := range []bool{false, true} {
			t.Run(fmt.Sprintf("skip=%v/compliant=%v", skip, safe), func(t *testing.T) {
				host := InstallDeps{Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if len(args) > 1 && args[0] == "--kube-context" {
						args = args[2:]
					}
					if args[0] == "list" {
						return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
					}
					if args[0] == "get" {
						return []byte(`{"controller":{"configMap":{"create":false}}}`), nil
					}
					t.Fatalf("review mutated before consent: %v", args)
					return nil, nil
				}}
				cfg := Config{Namespace: "oberth", ArgoNamespace: DefaultArgoNamespace, SkipArgo: skip, ArgoSecurityDefaults: true}
				result := InstallResult{OK: true}
				inspectInstallPreflight(t.Context(), &cfg, Deps{KubeClient: fake.NewClientset(argoSecurityLiveObjects(DefaultArgoNamespace, safe)...)}, host, "selected", &ClusterIdentity{}, &result, false)
				refused := false
				for _, check := range result.Checks {
					if check.Code == "argo_security_values" && !check.OK {
						refused = true
						for _, text := range []string{"v0.22.5 migration", "operator-config", `mainContainer.securityContext.allowPrivilegeEscalation=false`, `executor.securityContext.capabilities.drop=["ALL"]`} {
							if !strings.Contains(check.Remedy, text) {
								t.Fatal("missing ConfigMap remedy", text, check.Remedy)
							}
						}
					}
				}
				if refused == safe {
					t.Fatalf("live compliant=%v refused=%v checks=%v", safe, refused, result.Checks)
				}
			})
		}
	}
}

func TestArgoSecurityConsentIsNotSaved(t *testing.T) {
	name := filepath.Join(t.TempDir(), "answers.json")
	if err := WriteAnswers(name, Config{ArgoSecurityDefaults: true}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(name)
	if err != nil || bytes.Contains(raw, []byte("ArgoSecurityDefaults")) {
		t.Fatal("security consent persisted", err)
	}
	cfg, err := ReadAnswers(name)
	if err != nil || cfg.ArgoSecurityDefaults {
		t.Fatal("saved answers authorize changes", err)
	}
}

func TestArgoSecuritySkipRequiresConsentForExistingRelease(t *testing.T) {
	for _, consent := range []bool{false, true} {
		t.Run(map[bool]string{false: "no-consent", true: "explicit-consent"}[consent], func(t *testing.T) {
			client := fake.NewClientset(argoSecurityLiveObjects(DefaultArgoNamespace, false)...)
			writes := 0
			deps := Deps{KubeClient: client, Output: io.Discard, RunHelm: func(_ context.Context, args []string) ([]byte, error) {
				switch args[0] {
				case "list":
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				case "get":
					return []byte(`{"server":{"enabled":true},"controller":{"parallelism":9,"namespaceParallelism":5},"executor":{"resources":{"requests":{"cpu":"75m"}}}}`), nil
				case "repo":
					return nil, nil
				case "upgrade":
					writes++
					if len(args) < 3 || args[1] != "argo-workflows" || !slices.Contains(args, "--reuse-values") || slices.Contains(args, "--install") {
						t.Fatal("security consent caused broader install", args)
					}
					for i, arg := range args {
						if arg == "--set-json" && (i+1 == len(args) || !strings.Contains(args[i+1], "securityContext") && !strings.Contains(args[i+1], "podSecurityContext")) {
							t.Fatal("non-security value changed", args)
						}
						if arg == "--set" || arg == "--set-string" || arg == "--values" || arg == "--reset-values" {
							t.Fatal("operator values could be overwritten", args)
						}
					}
					setArgoSecurityLive(t, client, DefaultArgoNamespace, true)
					return nil, nil
				}
				t.Fatalf("unexpected command %v", args)
				return nil, nil
			}}
			result, err := InstallArgoWorkflows(t.Context(), Config{SkipArgo: true, ArgoSecurityDefaults: consent}, deps)
			if consent {
				if err != nil || writes != 1 || !result.Skipped || !result.Upgraded {
					t.Fatal(result, writes, err)
				}
			} else if err == nil || writes != 0 {
				t.Fatal("missing consent allowed reconcile", writes, err)
			}
		})
	}
}
