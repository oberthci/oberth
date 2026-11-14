package installer

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	kt "k8s.io/client-go/testing"
)

func TestPolicyProbeExecIgnoresInjectedDefaultContainer(t *testing.T) {
	kube := fake.NewClientset(preflightNode())
	probeNamespaceUIDFixture(kube)
	kube.PrependReactor("create", "pods", func(action kt.Action) (bool, runtime.Object, error) {
		pod := action.(kt.CreateAction).GetObject().(*corev1.Pod)
		pod.Annotations = map[string]string{"kubectl.kubernetes.io/default-container": "injected"}
		pod.Spec.Containers = append(pod.Spec.Containers, corev1.Container{Name: "injected", Image: "sidecar"})
		return readyPolicyModelPod(action)
	})
	model := &policyTrafficModel{kube: kube, mode: "egress-only", denial: "refused"}
	calls := map[string]int{}
	deps := testAdminDeps(Deps{KubeClient: kube, RunCommand: func(ctx context.Context, input []byte, name string, args ...string) ([]byte, error) {
		selected := "injected"
		for i, arg := range args {
			if arg == "--" {
				break
			}
			if arg == "--container" && i+1 < len(args) {
				selected = args[i+1]
			}
		}
		if selected != "probe" {
			t.Errorf("exec would run in injected default container: %v", args)
		}
		_, _, script, _ := policyModelArgs(args)
		// Only this wrapper mutates the counts; overlapping execs share it.
		model.mu.Lock()
		calls[script]++
		model.mu.Unlock()
		return model.run(ctx, input, name, args...)
	}})
	result := probeNetworkPolicy(t.Context(), Config{}, deps, policyModelTiming())
	if result.Status != "enforced" {
		t.Fatal(result)
	}
	for _, script := range []string{policyTrafficScript, policyOverlapScript, policyOverlapReadyScript, policyOverlapReleaseScript} {
		if calls[script] == 0 {
			t.Fatal("measurement did not exercise an exec phase")
		}
	}
}
