package installer

import (
	"context"
	"strings"
	"sync"
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
			attempts := 0
			var trafficMu sync.Mutex
			var policyCreated time.Time
			kube.PrependReactor("create", "networkpolicies", func(a k8stesting.Action) (bool, runtime.Object, error) {
				policyCreated = time.Now()
				return false, nil, nil
			})
			kube.PrependReactor("create", "pods", func(a k8stesting.Action) (bool, runtime.Object, error) {
				pod := a.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
				pod.Status = corev1.PodStatus{PodIP: "192.0.2.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
				return false, nil, nil
			})
			deps := Deps{KubeClient: kube, ContextName: "fixture", RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
				trafficMu.Lock()
				defer trafficMu.Unlock()
				attempts++
				podName := ""
				for i, arg := range args {
					if arg == "exec" && i+1 < len(args) {
						podName = args[i+1]
					}
				}
				ns := ""
				for i, arg := range args {
					if arg == "--namespace" && i+1 < len(args) {
						ns = args[i+1]
					}
				}
				pod, err := kube.CoreV1().Pods(ns).Get(ctx, podName, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				domain, err := kube.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
				if err != nil {
					return nil, err
				}
				policies, err := kube.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{})
				if err != nil {
					return nil, err
				}
				blocked := false
				for _, p := range policies.Items {
					for _, typ := range p.Spec.PolicyTypes {
						if mode == "ingress-only" && typ == networkingv1.PolicyTypeIngress {
							blocked = true
						}
						if mode == "egress-only" && typ == networkingv1.PolicyTypeEgress {
							blocked = true
						}
					}
					if mode == "programming-slower-than-window" && time.Since(policyCreated) >= 250*time.Millisecond {
						blocked = true
					}
				}
				if mode == "unrelated-http-outage" {
					blocked = attempts == 2 || attempts == 3
				} // Independent transient failure sequence.
				if mode == "namespace-exclusion" {
					blocked = len(policies.Items) > 0 && domain.Labels["domain"] != "pipeline"
				}
				if mode == "partial-node" {
					blocked = len(policies.Items) > 0 && pod.Spec.NodeName != "other-node"
				}
				if !strings.HasPrefix(podName, "client-") && mode != "unrelated-http-outage" {
					blocked = false
				}
				if blocked {
					return []byte("blocked"), nil
				}
				return []byte("allowed"), nil
			}}
			result := probeNetworkPolicy(t.Context(), Config{ArgoNamespace: "pipelines"}, deps, policyProbeTiming{time.Second, 50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond, time.Millisecond})
			want := "unverified"
			if mode == "egress-only" {
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

func TestControlOutageDuringClientRequestIsUnverified(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		pod := action.(k8stesting.CreateAction).GetObject().(*corev1.Pod)
		pod.Status = corev1.PodStatus{PodIP: "192.0.2.10", Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}
		return false, nil, nil
	})
	var inClient atomic.Bool
	deps := Deps{KubeClient: kube, ContextName: "chosen", RunCommand: func(ctx context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		name := ""
		for i, arg := range args {
			if arg == "exec" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		policies, err := kube.NetworkingV1().NetworkPolicies("").List(ctx, metav1.ListOptions{})
		if err != nil {
			return nil, err
		}
		if len(policies.Items) == 0 {
			return []byte("allowed"), nil
		}
		if strings.HasPrefix(name, "control-") {
			if inClient.Load() {
				return []byte("blocked"), nil
			}
			return []byte("allowed"), nil
		}
		inClient.Store(true)
		time.Sleep(20 * time.Millisecond)
		inClient.Store(false)
		return []byte("blocked"), nil
	}}
	result := probeNetworkPolicy(t.Context(), Config{}, deps, policyProbeTiming{time.Second, 50 * time.Millisecond, 100 * time.Millisecond, 50 * time.Millisecond, time.Millisecond})
	if result.Status != "unverified" {
		t.Fatal("a control outage during denial became enforcement evidence", result)
	}
}

func probeNamespaceUIDFixture(kube *fake.Clientset) {
	kube.PrependReactor("create", "namespaces", func(action k8stesting.Action) (bool, runtime.Object, error) {
		ns := action.(k8stesting.CreateAction).GetObject().(*corev1.Namespace)
		ns.UID = types.UID(ns.Name)
		return false, nil, nil
	})
}
