package installer_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func TestPendingReleaseRepair(t *testing.T) {
	t.Parallel()
	for _, deployed := range []bool{false, true} {
		for _, owned := range []bool{false, true} {
			t.Run(strings.Join([]string{boolText(deployed), boolText(owned)}, "/"), func(t *testing.T) {
				pendingCase(t, deployed, owned)
			})
		}
	}
}
func pendingCase(t *testing.T, deployed, owned bool) {
	t.Helper()
	f := setup(t)
	if !deployed {
		f.release = loadFixture(t, releaseFixture(t))
	}
	apply(t, f, prepare(t, f))
	f.charts.History.Latest = installer.Revision{Number: 2, Status: "pending-upgrade", Kind: "local", Version: f.release.Version(), Values: f.charts.History.Deployed.Values}
	if !deployed {
		f.charts.History.Deployed = installer.Revision{}
		f.charts.History.Latest.Kind = "verified"
	}
	if !owned {
		f.charts.History.Latest.Kind = "foreign"
	}
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if !owned {
		if err == nil {
			p.Close()
			t.Fatal("foreign pending release accepted")
		}
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !strings.Contains(strings.Join(p.Review(), "\n"), "Interrupted revision 2:") {
		t.Fatal(p.Review())
	}
	apply(t, f, p)
	if f.charts.Applies[len(f.charts.Applies)-1].Recovery == nil {
		t.Fatal("repair omitted")
	}
}

func boolText(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

func TestNativePendingRepairActions(t *testing.T) {
	t.Parallel()
	for _, deployed := range []bool{false, true} {
		t.Run(boolText(deployed), func(t *testing.T) {
			nativeRepairCase(t, deployed)
		})
	}
}

func nativeRepairCase(t *testing.T, deployed bool) {
	t.Helper()
	port, store, request := pendingFixture(t, deployed)
	if err := port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err := store.Last("oberth")
	if err != nil || !last.Info.Status.IsPending() {
		t.Fatal("preview repaired release", last, err)
	}
	if err = port.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err = store.Last("oberth")
	if err != nil || last.Info.Status != release.StatusDeployed {
		t.Fatal(last, err)
	}
	if last.Version != request.Recovery.Pending+1 {
		t.Fatal("repair did not upgrade from the pending revision", last.Version)
	}
}

func pendingFixture(t *testing.T, deployed bool) (installer.ChartLibrary, *storage.Storage, installer.ChartRequest) {
	t.Helper()
	store := storage.Init(driver.NewMemory())
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	config := &action.Configuration{Releases: store, KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, Capabilities: caps, Log: func(string, ...any) {}}
	port := installer.ChartForTest(config)
	ch := &chart.Chart{Metadata: &chart.Metadata{Name: "oberth", Version: "1.0.0", AppVersion: "1.0.0", APIVersion: "v2"}, Values: map[string]any{}, Schema: []byte(`{"type":"object"}`)}
	number := 1
	if deployed {
		if err := port.Apply(t.Context(), installer.ChartRequest{Chart: ch, Namespace: "oberth", Values: map[string]any{}}); err != nil {
			t.Fatal(err)
		}
		request := installer.ChartRequest{Chart: ch, Namespace: "oberth", Values: map[string]any{}}
		bindChartHistory(t, port, &request)
		if err := port.Apply(t.Context(), request); err != nil {
			t.Fatal(err)
		}
		number = 3
	}
	pending := &release.Release{Name: "oberth", Namespace: "oberth", Version: number, Chart: ch, Config: map[string]any{}, Info: &release.Info{Status: release.StatusPendingUpgrade}, Labels: installer.RevisionLabelsForTest(installer.ChartRequest{Chart: ch, Revision: number - 1})}
	if !deployed {
		pending.Info.Status = release.StatusPendingInstall
	}
	if err := store.Create(pending); err != nil {
		t.Fatal(err)
	}
	request := installer.ChartRequest{Chart: ch, Namespace: "oberth", Revision: number, Values: map[string]any{}, Recovery: &installer.Recovery{Pending: number}}
	if deployed {
		request.Recovery.Deployed = 2
	}
	bindChartHistory(t, port, &request)
	return port, store, request
}

type interruptedChart struct {
	installer.ChartPort
	history *installer.History
}

func (c interruptedChart) Apply(_ context.Context, r installer.ChartRequest) error {
	c.history.Latest = installer.Revision{Number: r.Revision + 1, Status: "pending-install", Kind: r.Kind(), Version: r.Chart.Metadata.AppVersion, Values: r.Values}
	return context.Canceled
}
func TestCancelDuringChartCallIsRepairable(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.engine.Charts = interruptedChart{ChartPort: f.charts, history: &f.charts.History}
	p := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.engine.Charts = f.charts
	rerun := prepare(t, f)
	if !strings.Contains(strings.Join(rerun.Review(), "\n"), "retry with this chart") {
		t.Fatal(rerun.Review())
	}
	apply(t, f, rerun)
	if f.charts.History.Latest.Status != "deployed" {
		t.Fatal(f.charts.History)
	}
}

func TestBundledPendingPreviewIsReadOnly(t *testing.T) {
	t.Parallel()
	port, store, request := pendingFixture(t, false)
	ch, err := loader.Load(chartPath())
	if err != nil {
		t.Fatal(err)
	}
	request.Chart = ch
	if err = port.Preview(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	last, err := store.Last("oberth")
	if err != nil || !last.Info.Status.IsPending() {
		t.Fatal("preview repaired release", last, err)
	}
}
