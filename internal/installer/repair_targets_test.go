package installer_test

import (
	"bytes"
	"encoding/json"
	"io"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
)

func TestFreshInstallRepairDoesNotRunStoredHooks(t *testing.T) {
	t.Parallel()
	_, store, request := pendingFixture(t, false)
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	poisonHistoryTarget(last, "hook")
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	checked := loadFixture(t, releaseFixture(t))
	request.Chart = installer.ReleaseChartForTest(checked)
	kc := &historyTargetKube{PrintingKubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	if err = port.Apply(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	if kc.injected {
		t.Fatal("first-install repair ran stored hooks")
	}
	last, err = store.Last("oberth")
	if err != nil || last.Version != 2 || last.Info.Status != release.StatusDeployed {
		t.Fatal("first-install repair did not apply fresh target", err)
	}
}
func TestNativeRepairValidatesFreshChartBeforeHistoryWrite(t *testing.T) {
	t.Parallel()
	port, store, request := pendingFixture(t, true)
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	request.Chart.Schema = []byte(`{"type":"object","required":["missing"]}`)
	before, err := json.Marshal(last)
	if err != nil {
		t.Fatal(err)
	}
	if err = port.Apply(t.Context(), request); err == nil {
		t.Fatal("invalid fresh chart accepted")
	}
	last, err = store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	after, err := json.Marshal(last)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("invalid fresh chart changed history", err)
	}
}
