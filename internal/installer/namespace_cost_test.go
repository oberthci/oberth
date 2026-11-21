package installer

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/kubeconfig"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestNamespaceMembershipCostFor200Releases(t *testing.T) {
	t.Parallel()
	client := namespaceCostClient(t)
	inventory, err := client.CoreV1().Secrets("").List(t.Context(), metav1.ListOptions{LabelSelector: "owner=helm"})
	if err != nil {
		t.Fatal(err)
	}
	records, err := namespaceReleaseRecords(inventory)
	if err != nil || len(records) != 600 || len(currentNamespaceRevisions(records)) != 400 {
		t.Fatal("revision scan is not bounded to latest and deployed", len(records), err)
	}
	engine := &Engine{Cluster: kubeconfig.Target{Client: client}}
	check := func(tb testing.TB) {
		client.ClearActions()
		if err := engine.checkNamespaceReleases(t.Context(), Choices{Namespace: "server", PipelineNamespace: "runs"}); err != nil {
			tb.Fatal(err)
		}
		actions := client.Actions()
		if len(actions) != 1 || !actions[0].Matches("list", "secrets") || actions[0].GetNamespace() != "" {
			tb.Fatal("membership performs repeated release reads", actions)
		}
	}
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			check(b)
		}
	})
	t.Logf("200 releases, 600 stored revisions: one Secret list, each revision decoded once, 400 manifests inspected; %s; %s", result.String(), result.MemString())
}

func namespaceCostClient(t *testing.T) *kubefake.Clientset {
	t.Helper()
	client := kubefake.NewSimpleClientset()
	store := driver.NewSecrets(client.CoreV1().Secrets("control"))
	for i := range 200 {
		for version, status := range []release.Status{release.StatusSuperseded, release.StatusDeployed, release.StatusFailed} {
			r := &release.Release{Name: fmt.Sprintf("fixture-%d", i), Namespace: "control", Version: version + 1, Info: &release.Info{Status: status}, Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "fixture-chart"}}, Manifest: fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: fixture-%d\n", i)}
			if err := store.Create(fmt.Sprintf("sh.helm.release.v1.%s.v%d", r.Name, r.Version), r); err != nil {
				t.Fatal(err)
			}
		}
	}
	return client
}
