package installer_test

import (
	"fmt"
	"testing"

	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
	ktesting "k8s.io/client-go/testing"
)

func TestApplyReads300ReleasesOnce(t *testing.T) {
	f := setup(t)
	records := driver.NewSecrets(f.client.CoreV1().Secrets("other"))
	for i := range 300 {
		r := &release.Release{Name: fmt.Sprintf("other-%d", i), Namespace: "other", Version: 1, Info: &release.Info{Status: release.StatusDeployed}, Chart: &chart.Chart{Metadata: &chart.Metadata{Name: "other-chart"}}, Manifest: fmt.Sprintf("apiVersion: v1\nkind: Namespace\nmetadata:\n  name: other-%d\n", i)}
		if err := records.Create(fmt.Sprintf("sh.helm.release.v1.%s.v1", r.Name), r); err != nil {
			t.Fatal(err)
		}
	}

	preparation, reads := 0, 0
	measurement := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			b.StopTimer()
			f.client.ClearActions()
			plan := prepare(t, f)
			preparation = clusterReleaseLists(f.client.Actions())
			f.client.ClearActions()
			b.StartTimer()
			apply(t, f, plan)
			b.StopTimer()
			reads = clusterReleaseLists(f.client.Actions())
			plan.Close()
			b.StartTimer()
		}
	})
	t.Logf("300 releases: prepare lists=%d; apply lists=%d; %s; %s", preparation, reads, measurement.String(), measurement.MemString())
	if preparation != 1 || reads != 1 {
		t.Fatalf("duplicate cluster inventory: prepare=%d apply=%d", preparation, reads)
	}

}

func clusterReleaseLists(actions []ktesting.Action) int {
	count := 0
	for _, action := range actions {
		if action.Matches("list", "secrets") && action.GetNamespace() == "" {
			count++
		}
	}
	return count
}
