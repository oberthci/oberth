package installer_test

import (
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/storage"
	clientfake "k8s.io/client-go/kubernetes/fake"
)

func TestVerifiedEarlierChartApplyContract(t *testing.T) {
	t.Parallel()
	for _, factory := range []struct {
		name string
		new  func() installer.ChartPort
	}{
		{"fake", func() installer.ChartPort { return &fake.Chart{} }},
		{"library", func() installer.ChartPort {
			caps := chartutil.DefaultCapabilities.Copy()
			caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
			secrets := clientfake.NewClientset().CoreV1().Secrets("oberth")
			store := storage.Init(installer.ReleaseSecretsForTest(secrets))
			t.Cleanup(func() {
				last, err := store.Last("oberth")
				if err != nil {
					t.Error(err)
					return
				}
				for _, name := range []string{"oberth-controller-workflow-controller", "oberth-store", "oberth-source-plugin@sha256:" + strings.Repeat("c", 64)} {
					if !strings.Contains(last.Manifest, name) {
						t.Error("verified earlier apply omitted bundled workload", name)
					}
				}
			})
			return installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, Capabilities: caps, Log: func(string, ...any) {}})
		}},
	} {
		t.Run(factory.name, func(t *testing.T) { verifiedEarlierApply(t, factory.new()) })
	}
}
func verifiedEarlierApply(t *testing.T, port installer.ChartPort) {
	t.Helper()
	// Real signed earlier archive, including native dependencies; history itself
	// is serialized through the Secrets driver in the library case.
	earlier := loadFixture(t, releaseFor(t, "v0.0.9", 1))
	current := loadFixture(t, releaseFixture(t))
	for _, r := range []installer.Release{earlier, current} {
		request := installer.ChartRequest{Chart: installer.ReleaseChartForTest(r), Namespace: "oberth", Values: map[string]any{}}
		bindChartHistory(t, port, &request)
		if err := port.Apply(t.Context(), request); err != nil {
			t.Fatal(err)
		}
	}
	request := installer.ChartRequest{Chart: installer.ReleaseChartForTest(earlier), Namespace: "oberth", Revision: 2, Values: map[string]any{"database": map[string]any{"file": "/var/lib/oberth/restored.db"}}, RollbackID: "owned-operation"}
	bindChartHistory(t, port, &request)
	request.Revision = 77
	if err := port.Apply(t.Context(), request); err == nil {
		t.Fatal("unrelated revision accepted")
	}
	request.Revision = 2
	if err := port.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	h, err := port.Read(t.Context(), "oberth")
	if err != nil || h.Latest.Version != "v0.0.9" || h.Latest.RollbackID != "owned-operation" {
		t.Fatal(h, err)
	}
	if h.Latest.Values["database"].(map[string]any)["file"] != "/var/lib/oberth/restored.db" {
		t.Fatal("database was not repointed")
	}
}
