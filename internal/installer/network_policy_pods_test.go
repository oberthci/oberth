package installer

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func replacePolicyModelPod(t *testing.T, kube *fake.Clientset, namespace, name string) {
	t.Helper()
	resource := corev1.SchemeGroupVersion.WithResource("pods")
	object, err := kube.Tracker().Get(resource, namespace, name)
	if err != nil {
		t.Error(err)
		return
	}
	replacement := object.(*corev1.Pod).DeepCopy()
	replacement.UID = types.UID("replacement-" + name)
	if err := kube.Tracker().Delete(resource, namespace, name); err != nil {
		t.Error(err)
		return
	}
	if err := kube.Tracker().Create(resource, replacement, namespace); err != nil {
		t.Error(err)
	}
}

func TestPolicyProbeRejectsSameNamePodReplacement(t *testing.T) {
	for _, name := range []string{"server", "client-0", "control-0"} {
		for _, phase := range []string{"readiness", "baseline exec", "selected round"} {
			t.Run(name+"/"+phase, func(t *testing.T) {
				kube := fake.NewClientset(preflightNode())
				probeNamespaceUIDFixture(kube)
				kube.PrependReactor("create", "pods", readyPolicyModelPod)
				var once sync.Once
				var changed atomic.Bool
				var calls atomic.Int32
				if phase == "readiness" {
					kube.PrependReactor("get", "pods", func(a kt.Action) (bool, runtime.Object, error) {
						if a.(kt.GetAction).GetName() == name {
							once.Do(func() { replacePolicyModelPod(t, kube, a.GetNamespace(), name); changed.Store(true) })
						}
						return false, nil, nil
					})
				}
				model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "refused"}
				deps := Deps{KubeClient: kube, RunCommand: func(ctx context.Context, input []byte, command string, args ...string) ([]byte, error) {
					calls.Add(1)
					namespace, _, script, _ := policyModelArgs(args)
					if phase != "readiness" && script == policyTrafficScript {
						policies, err := kube.NetworkingV1().NetworkPolicies(namespace).List(ctx, metav1.ListOptions{})
						if err != nil {
							return nil, err
						}
						if phase == "baseline exec" || len(policies.Items) > 0 {
							once.Do(func() { replacePolicyModelPod(t, kube, namespace, name); changed.Store(true) })
						}
					}
					return model.run(ctx, input, command, args...)
				}}
				result := probeNetworkPolicy(t.Context(), Config{}, deps, policyModelTiming())
				if !changed.Load() || result.Status != "unverified" || !strings.Contains(result.Remedy, "identity changed") {
					t.Fatal("replacement supplied measurement evidence", result)
				}
				if phase == "readiness" && calls.Load() != 0 {
					t.Fatal("exec ran after readiness observed a different UID")
				}
			})
		}
	}
}

func TestPolicyProbeRejectsServerReplacementAtFinalRecovery(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", readyPolicyModelPod)
	var removals, recovery atomic.Int32
	kube.PrependReactor("delete", "networkpolicies", func(kt.Action) (bool, runtime.Object, error) { removals.Add(1); return false, nil, nil })
	model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "refused"}
	changed := false
	deps := Deps{KubeClient: kube, RunCommand: func(ctx context.Context, input []byte, command string, args ...string) ([]byte, error) {
		namespace, pod, script, _ := policyModelArgs(args)
		raw, err := model.run(ctx, input, command, args...)
		if script == policyTrafficScript && pod == "control-0" && removals.Load() == policyProbeRounds && recovery.Add(1) == policyProbeSamples {
			replacePolicyModelPod(t, kube, namespace, "server")
			changed = true
		}
		return raw, err
	}}
	result := probeNetworkPolicy(t.Context(), Config{}, deps, policyModelTiming())
	if !changed || result.Status != "unverified" || !strings.Contains(result.Remedy, "identity changed for server") {
		t.Fatal("last recovery accepted a replaced server", result)
	}
}

func TestPolicyProbeRequiresCreatedPodUID(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", func(a kt.Action) (bool, runtime.Object, error) {
		readyPolicyModelPod(a)
		a.(kt.CreateAction).GetObject().(*corev1.Pod).UID = ""
		return false, nil, nil
	})
	result := probeNetworkPolicy(t.Context(), Config{}, Deps{KubeClient: kube, RunCommand: func(context.Context, []byte, string, ...string) ([]byte, error) {
		t.Error("exec without a created UID")
		return nil, nil
	}}, policyModelTiming())
	if result.Status != "unverified" || !strings.Contains(result.Remedy, "no verified UID") {
		t.Fatal(result)
	}
}
