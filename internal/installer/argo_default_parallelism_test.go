package installer

import (
	"context"
	"io"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func cpuNode(name, cpu string) *corev1.Node {
	return &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: name}, Status: corev1.NodeStatus{
		Allocatable: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}}
}

func TestDefaultArgoParallelismFollowsAllocatableCPU(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		nodes []*corev1.Node
		want  int
		ok    bool
	}{
		{[]*corev1.Node{cpuNode("a", "12")}, 6, true},
		{[]*corev1.Node{cpuNode("a", "2")}, 2, true},
		{[]*corev1.Node{cpuNode("a", "3500m"), cpuNode("b", "4")}, 3, true},
		{nil, 0, false},
	} {
		client := fake.NewClientset()
		for _, node := range tc.nodes {
			if _, err := client.CoreV1().Nodes().Create(t.Context(), node, metav1.CreateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		got, ok := defaultArgoParallelism(t.Context(), Deps{KubeClient: client})
		if got != tc.want || ok != tc.ok {
			t.Errorf("nodes %d: got %d,%t want %d,%t", len(tc.nodes), got, ok, tc.want, tc.ok)
		}
	}
}

// A fresh controller gets the default ceiling; an explicit flag (0 = no
// limit) and an existing release keep theirs.
func TestFreshArgoInstallSetsDefaultParallelism(t *testing.T) {
	t.Parallel()
	run := func(cfg Config, existing bool) []string {
		var install []string
		deps := Deps{Output: io.Discard, KubeClient: fake.NewClientset(cpuNode("a", "8")), RunHelm: func(_ context.Context, args []string) ([]byte, error) {
			switch args[0] {
			case "list":
				if existing {
					return []byte(`[{"name":"argo-workflows","chart":"argo-workflows-1.0.24","status":"deployed"}]`), nil
				}
				return []byte(`[]`), nil
			case "get":
				return []byte(`{}`), nil
			case "upgrade":
				install = slices.Clone(args)
			}
			return nil, nil
		}}
		if _, err := InstallArgoWorkflows(t.Context(), cfg, deps); err != nil {
			t.Fatal(err)
		}
		return install
	}
	if args := strings.Join(run(Config{}, false), " "); !strings.Contains(args, "controller.parallelism=4") {
		t.Fatalf("fresh install without the default ceiling: %s", args)
	}
	zero := 0
	if args := strings.Join(run(Config{ArgoParallelism: &zero}, false), " "); !strings.Contains(args, "controller.parallelism=0") || strings.Contains(args, "parallelism=4") {
		t.Fatalf("explicit 0 overridden: %s", args)
	}
	if args := strings.Join(run(Config{Upgrade: true}, true), " "); strings.Contains(args, "controller.parallelism") {
		t.Fatalf("existing release got a default: %s", args)
	}
}
