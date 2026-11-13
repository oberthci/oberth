package installer

import (
	"context"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func TestProbeEvidenceRequiresEgressControlAndConvergence(t *testing.T) {
	for _, mode := range []string{"ingress-only", "egress-only", "unrelated-http-outage", "programming-slower-than-window", "namespace-exclusion", "partial-node"} {
		t.Run(mode, func(t *testing.T) {
			node := preflightNode()
			other := node.DeepCopy()
			other.Name = "other-node"
			pipeline := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "pipelines", Labels: map[string]string{"domain": "pipeline"}}}
			kube := fake.NewClientset(node, other, pipeline)
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			model := &policyTrafficModel{kube: kube, mode: mode, denial: "timeout"}
			if mode == "unrelated-http-outage" {
				model.mode = "absent"
				model.fault = func(_ string, n int) string {
					if n == 2 || n == 3 {
						return "refused"
					}
					return ""
				}
			}
			deps := Deps{KubeClient: kube, ContextName: "fixture", RunCommand: model.run}
			result := probeNetworkPolicy(t.Context(), Config{ArgoNamespace: "pipelines"}, deps, policyModelTiming())
			want := "unverified"
			if mode == "egress-only" || mode == "namespace-exclusion" {
				want = "enforced"
			}
			if result.Status != want {
				t.Fatalf("got %s want %s", result.Status, want)
			}
			if !strings.Contains(result.Evidence, "pipeline namespace pipelines") || !strings.Contains(result.Evidence, "other-node") || !strings.Contains(result.Evidence, "name-specific exclusions") {
				t.Fatal("missing coverage scope", result)
			}
			if mode == "programming-slower-than-window" && (!strings.Contains(result.Remedy, "convergence") || !strings.Contains(result.Remedy, "after")) {
				t.Fatal("missing delay remedy", result)
			}
			if mode == "namespace-exclusion" && !strings.Contains(result.Evidence, "namespace-label-specific exclusions") {
				t.Fatal("omitted pipeline label scope was not disclosed", result)
			}
			t.Logf("%s -> %s", mode, result.Status)
		})
	}
}

func TestProbeReconcilesStaleNamespacesAndProtectsActiveAndForeign(t *testing.T) {
	kube := fake.NewClientset()
	makeNS := func(token string, expiry time.Time) *corev1.Namespace {
		return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "oberth-policy-probe-" + token, UID: types.UID(token), Labels: map[string]string{policyProbeLabel: token}, Annotations: map[string]string{policyProbeExpiry: expiry.UTC().Format(time.RFC3339)}}}
	}
	stale := makeNS("0123456789abcdef", time.Now().Add(-time.Minute))
	active := makeNS("1123456789abcdef", time.Now().Add(time.Hour))
	foreign := makeNS("2123456789abcdef", time.Now().Add(-time.Hour))
	foreign.Name = "unrelated"
	for _, ns := range []*corev1.Namespace{stale, active, foreign} {
		if _, err := kube.CoreV1().Namespaces().Create(t.Context(), ns, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	kube.PrependReactor("delete", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		deletion := action.(k8stesting.DeleteAction)
		if deletion.GetName() != stale.Name || deletion.GetDeleteOptions().Preconditions == nil || *deletion.GetDeleteOptions().Preconditions.UID != stale.UID {
			t.Fatal("unsafe stale cleanup", action)
		}
		return false, nil, nil
	})
	evidence, err := reconcilePolicyProbes(t.Context(), Deps{KubeClient: kube}, policyProbeTiming{interval: time.Millisecond})
	if err != nil || !strings.Contains(evidence, active.Name) {
		t.Fatal("active leftover not reported", evidence, err)
	}
	if _, err := kube.CoreV1().Namespaces().Get(t.Context(), stale.Name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatal("stale probe survived", err)
	}
	for _, ns := range []*corev1.Namespace{active, foreign} {
		if _, err := kube.CoreV1().Namespaces().Get(t.Context(), ns.Name, metav1.GetOptions{}); err != nil {
			t.Fatal("active/foreign probe removed", err)
		}
	}
}

func probeNamespaceUIDFixture(kube *fake.Clientset) {
	probePolicyUIDFixture(kube)
	kube.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		ns.UID = types.UID(ns.Name)
		return false, nil, nil
	})
}

func readyPolicyModelPod(action k8stesting.Action) (bool, runtime.Object, error) {
	pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
	pod.UID = types.UID(pod.Name + "-uid")
	pod.Status = corev1.PodStatus{PodIP: "192.0.2.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
	return false, nil, nil
}

func TestProbeEvidenceRejectsAndRecovers(t *testing.T) {
	for _, tc := range []struct{ name, denial, want string }{
		{"drop", "timeout", "enforced"}, {"reject", "refused", "enforced"}, {"reset", "reset", "enforced"},
		{"refused baseline", "refused", "unverified"}, {"refused peer baseline", "refused", "unverified"}, {"never recovers", "refused", "unverified"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: tc.denial}
			if tc.name == "refused baseline" {
				model.fault = func(string, int) string { return "refused" }
			}
			if tc.name == "refused peer baseline" {
				model.fault = func(source string, _ int) string {
					if !policyModelSourceA(source) {
						return "refused"
					}
					return ""
				}
			}
			if tc.name == "never recovers" {
				model.fault = func(_ string, n int) string {
					if n > 6 {
						return "refused"
					}
					return ""
				}
			}
			result := probeNetworkPolicy(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming())
			if result.Status != tc.want {
				t.Fatalf("got %+v, want %s", result, tc.want)
			}
			if tc.name == "refused peer baseline" && !strings.Contains(result.Remedy, "Client control-0: connection refused") {
				t.Fatal("wrong failed source", result)
			}
			if tc.want == "enforced" {
				if !strings.Contains(result.Evidence, "selects client") || !strings.Contains(result.Evidence, "selects control") || strings.Count(result.Evidence, "2 paired denial measurements") != policyProbeRounds || !strings.Contains(result.Evidence, "Measured egress enforcement") || !strings.Contains(result.Evidence, "cannot prove causality") || !strings.Contains(result.Evidence, "including owned cleanup") {
					t.Fatal("missing selection/overlap evidence", result)
				}
			} else if !strings.Contains(result.Remedy, "connection refused") {
				t.Fatal("missing observed failure", result)
			}
		})
	}
}

// The review's independent source fault: no dataplane, no policy reads in the
// traffic failure function. It must fail for either randomized round order.
func TestReview1548RejectingSourceWithoutEnforcement(t *testing.T) {
	for _, failure := range []string{"refused", "reset"} {
		t.Run(failure, func(t *testing.T) {
			kube := fake.NewClientset(preflightNode())
			probeNamespaceUIDFixture(kube)
			kube.PrependReactor("create", "pods", readyPolicyModelPod)
			model := &policyTrafficModel{kube: kube, mode: "absent", denial: failure, fault: func(source string, n int) string {
				if policyModelSourceA(source) && (n-1)/3%2 == 1 {
					return failure
				}
				return ""
			}}
			result := probeNetworkPolicy(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming())
			if result.Status != "unverified" {
				t.Fatal("unrelated per-source burst became enforcement", result)
			}
		})
	}
}

func TestReview1548DuringSampleCanHappenOnlyAfter(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
	defer cancel()
	var clients atomic.Int32
	deps := Deps{RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		_, _, script, _ := policyModelArgs(args)
		switch script {
		case policyOverlapScript:
			<-ctx.Done()
			return nil, ctx.Err()
		case policyOverlapReadyScript:
			return []byte("pending"), nil
		default:
			clients.Add(1)
			return []byte("refused"), nil
		}
	}}
	pods := policyPairPods(t)
	deps.KubeClient = pods.kube
	state, err := policyProbePair(ctx, Config{}, deps, policyProbeTiming{interval: time.Millisecond}, pods.identities, "client-0", "control-0", "http://192.0.2.10:8080")
	if err == nil || state != "" || clients.Load() != 0 {
		t.Fatal("attempt ran before a real overlap handshake", state, err, clients.Load())
	}
}

func TestPolicyOverlapRejectsEarlyCompletionAndControlFailure(t *testing.T) {
	for _, mode := range []string{"early completion", "failed completion"} {
		t.Run(mode, func(t *testing.T) {
			var reads atomic.Int32
			release := make(chan struct{})
			deps := Deps{RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				_, _, script, _ := policyModelArgs(args)
				switch script {
				case policyOverlapScript:
					select {
					case <-release:
						return []byte("refused"), nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				case policyOverlapReleaseScript:
					close(release)
					return []byte("released"), nil
				case policyOverlapReadyScript:
					n := reads.Add(1)
					if n == 2 {
						if mode == "early completion" {
							return []byte("closed"), nil
						}
					}
					return []byte("open"), nil
				default:
					_, pod, _, _ := policyModelArgs(args)
					if pod == "control-0" {
						return []byte("allowed"), nil
					}
					return []byte("refused"), nil
				}
			}}
			pods := policyPairPods(t)
			deps.KubeClient = pods.kube
			state, err := policyProbePair(t.Context(), Config{}, deps, policyProbeTiming{interval: time.Millisecond}, pods.identities, "client-0", "control-0", "http://192.0.2.10:8080")
			if err == nil || state != "" {
				t.Fatal("missing overlap/control success accepted", state, err)
			}
		})
	}
}

func TestPolicyRecoveryConvergesAfterTransientSourceFailures(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	namespace := "scratch"
	_, err := kube.CoreV1().Namespaces().Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}, metav1.CreateOptions{})
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"server", "client-0", "control-0"}
	for _, name := range names {
		pod := policyProbePod(namespace, "token", "client", "image")
		pod.Name = name
		pod.UID = types.UID(name + "-uid")
		pod.Status = corev1.PodStatus{PodIP: "192.0.2.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		if _, err := kube.CoreV1().Pods(namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	model := &policyTrafficModel{kube: kube, mode: "absent", fault: func(_ string, n int) string {
		if n == 1 {
			return "refused"
		}
		return ""
	}}
	if err := waitPolicyTraffic(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: model.run}, policyModelTiming(), policyProbePods{namespace: namespace, names: names, uids: map[string]types.UID{"server": "server-uid", "client-0": "client-0-uid", "control-0": "control-0-uid"}}, "http://192.0.2.10/", ""); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyFreshUnselectedFailureBeforeAndAfter(t *testing.T) {
	for _, tc := range []struct {
		phase   string
		request int
	}{{"before", 2}, {"after", 3}} {
		t.Run(tc.phase, func(t *testing.T) {
			model := &policyTrafficModel{mode: "absent", fault: func(source string, n int) string {
				if policyModelSourceA(source) {
					return "reset"
				}
				if n == tc.request {
					return "refused"
				}
				return ""
			}}
			pods := policyPairPods(t)
			state, err := policyProbePair(t.Context(), Config{}, Deps{KubeClient: pods.kube, RunCommand: model.run}, policyModelTiming(), pods.identities, "client-0", "control-0", "http://192.0.2.10/")
			if err == nil || state != "" || !strings.Contains(policyProbeFailure(err), "Fresh unselected connection "+tc.phase) || !strings.Contains(policyProbeFailure(err), "connection refused") {
				t.Fatal(state, err)
			}
			if len(model.streams) != 0 {
				t.Fatal("held response worker not joined")
			}
		})
	}
}

type policyPairFixture struct {
	kube       *fake.Clientset
	identities policyProbePods
}

func policyPairPods(t *testing.T) policyPairFixture {
	t.Helper()
	kube := fake.NewClientset()
	pods := policyProbePods{namespace: "scratch", names: []string{"server", "client-0", "control-0"}, uids: map[string]types.UID{}}
	for _, name := range pods.names {
		pod := policyProbePod(pods.namespace, "token", "client", "image")
		pod.Name = name
		pod.UID = types.UID(name + "-uid")
		if _, err := kube.CoreV1().Pods(pods.namespace).Create(t.Context(), pod, metav1.CreateOptions{}); err != nil {
			t.Fatal(err)
		}
		pods.uids[name] = pod.UID
	}
	return policyPairFixture{kube, pods}
}

func probePolicyUIDFixture(kube *fake.Clientset) {
	sequence := 0
	kube.PrependReactor("create", "networkpolicies", func(action k8stesting.Action) (bool, runtime.Object, error) {
		policy := action.(k8stesting.CreateAction).GetObject().(*networkingv1.NetworkPolicy)
		sequence++
		policy.UID = types.UID(policy.Name + "-" + strconv.Itoa(sequence))
		return false, nil, nil
	})
}
