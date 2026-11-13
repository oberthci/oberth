package installer

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	kt "k8s.io/client-go/testing"
)

func TestPlanNeverMutatesCluster(t *testing.T) {
	for _, stored := range []string{"fresh", "true", "false", "missing"} {
		t.Run(stored, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode(), preflightStorage())
			kube.PrependReactor("*", "*", func(a kt.Action) (bool, runtime.Object, error) {
				if a.GetVerb() != "get" && a.GetVerb() != "list" {
					t.Errorf("plan attempted mutation: %s %s", a.GetVerb(), a.GetResource())
					return true, nil, errors.New("mutation refused")
				}
				return false, nil, nil
			})
			ca := newTestServerCA(t, []string{"api.example.test"})
			host := InstallDeps{Output: io.Discard,
				LookPath: func(name string) (string, error) { return name, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				ProbeNetworkPolicy: func(context.Context, Config, Deps) NetworkPolicyProbeResult {
					t.Fatal("plan invoked live enforcement measurement")
					return NetworkPolicyProbeResult{}
				},
				RunCommand: func(ctx context.Context, in []byte, name string, args ...string) ([]byte, error) {
					if name != "kubectl" || len(args) < 4 || args[0] != "--context" || args[1] != "chosen" || args[2] != "get" {
						t.Fatalf("plan subprocess must read the chosen context: %s %v", name, args)
					}
					return preflightStats(ctx, in, name, args...)
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if len(args) < 3 || args[0] != "--kube-context" || args[1] != "chosen" {
						t.Fatalf("Helm lost chosen context: %v", args)
					}
					switch args[2] {
					case "list":
						if stored == "fresh" {
							return []byte("[]"), nil
						}
						return []byte(`[{"name":"oberth","namespace":"oberth","status":"deployed","chart":"oberth-0.22.2"}]`), nil
					case "get":
						if stored == "missing" {
							return []byte(`{}`), nil
						}
						return []byte(`{"networkPolicy":{"enabled":` + stored + `}}`), nil
					default:
						t.Fatalf("plan ran mutating Helm command: %v", args)
						return nil, nil
					}
				},
			}
			plan := PlanInstall(t.Context(), Config{ContextName: "chosen"}, host)
			if !plan.OK {
				t.Fatalf("read-only plan failed: %+v", plan.Checks)
			}
			expected := stored
			if stored == "fresh" {
				expected = "true"
			}
			if stored == "missing" {
				expected = "unspecified (preserved)"
			}
			if plan.Review.NetworkPolicy != expected {
				t.Fatalf("stored policy changed: %+v", plan.Review)
			}
			found := false
			for _, check := range plan.Checks {
				if check.Code == "network_policy" {
					found = true
					if check.OK || !check.Warning || check.Reason != "unverified" || check.Evidence != "" || !strings.Contains(check.Remedy, "first at install, after approval") {
						t.Fatalf("plan claimed measurement: %+v", check)
					}
				}
			}
			if !found {
				t.Fatal("plan omitted deferred measurement")
			}
		})
	}
}

func TestApplyMeasuresBeforeReleaseChangesAndCleansUp(t *testing.T) {
	for _, mode := range []string{"absent", "unverified", "enforced", "cleanup failure"} {
		t.Run(mode, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode(), preflightStorage())
			measured := 0
			stopped := errors.New("fixture stops after successful gate")
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			kube.PrependReactor("*", "*", func(a kt.Action) (bool, runtime.Object, error) {
				if a.GetVerb() == "get" || a.GetVerb() == "list" {
					return false, nil, nil
				}
				namespace := a.GetNamespace()
				if a.GetResource().Resource == "namespaces" {
					if a.GetVerb() == "create" {
						namespace = a.(kt.CreateAction).GetObject().(*corev1.Namespace).Name
					}
					if a.GetVerb() == "delete" {
						namespace = a.(kt.DeleteAction).GetName()
					}
				}
				if !strings.HasPrefix(namespace, "oberth-policy-probe-") {
					if mode == "enforced" && measured == 1 {
						return true, nil, stopped
					}
					t.Fatalf("release mutation before probe gate: %v", a)
				}
				return false, nil, nil
			})
			if mode == "unverified" {
				kube.PrependReactor("create", "pods", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "server", errors.New("fixture"))
				})
			}
			if mode == "cleanup failure" {
				kube.PrependReactor("delete", "namespaces", func(kt.Action) (bool, runtime.Object, error) {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "namespaces"}, "scratch", errors.New("fixture"))
				})
			}
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "timeout"}
			if mode == "absent" {
				model.mode = "absent"
			}
			ca := newTestServerCA(t, []string{"api.example.test"})
			host := InstallDeps{Output: io.Discard, CredentialSink: func(string, string) {},
				LookPath: func(name string) (string, error) { return name, nil },
				LoadKubeConfig: func(string) (kubernetes.Interface, *rest.Config, string, error) {
					return kube, &rest.Config{Host: "https://api.example.test", TLSClientConfig: rest.TLSClientConfig{CAData: ca}}, "chosen", nil
				},
				RunCommand: preflightStats,
				ProbeNetworkPolicy: func(ctx context.Context, cfg Config, deps Deps) NetworkPolicyProbeResult {
					measured++
					deps.RunCommand = model.run
					return probeNetworkPolicy(ctx, cfg, deps, policyModelTiming())
				},
				RunHelm: func(_ context.Context, args []string) ([]byte, error) {
					if slices.Contains(args, "list") {
						return []byte("[]"), nil
					}
					t.Fatalf("Helm changed release before enforcement: %v", args)
					return nil, nil
				},
			}
			err := Execute(t.Context(), Config{ContextName: "chosen", ChartPath: testLocalChart, ClientAccess: "none"}, host)
			if measured != 1 {
				t.Fatalf("apply did not measure once: %d %v", measured, err)
			}
			if mode == "enforced" {
				if !errors.Is(err, stopped) {
					t.Fatalf("enforced apply did not advance: %v", err)
				}
			} else if !errors.Is(err, errInstallPreflight) || !strings.Contains(err.Error(), "network_policy:") {
				t.Fatalf("apply accepted non-enforcement: %v", err)
			}
			remaining, listErr := kube.CoreV1().Namespaces().List(t.Context(), metav1.ListOptions{})
			if listErr != nil {
				t.Fatal(listErr)
			}
			want := 0
			if mode == "cleanup failure" {
				want = 1
			}
			if len(remaining.Items) != want {
				t.Fatalf("probe cleanup: %v", remaining.Items)
			}
			deleted := false
			for _, a := range kube.Actions() {
				if a.GetVerb() == "delete" && a.GetResource().Resource == "namespaces" {
					deleted = true
				}
			}
			if !deleted {
				t.Fatal("apply did not attempt scratch cleanup")
			}
		})
	}
}
