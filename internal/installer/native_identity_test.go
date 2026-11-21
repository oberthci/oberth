package installer

import (
	"errors"
	"io"
	"testing"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart"
	kubefake "helm.sh/helm/v3/pkg/kube/fake"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func TestNativeRepairAndRollbackRequireBoundInstallerRevision(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"repair", "rollback"} {
		t.Run(operation, func(t *testing.T) {
			config, request := stalePendingBinding(t)
			var err error
			if operation == "repair" {
				err = (ChartLibrary{}).repair(t.Context(), config, request)
			} else {
				err = (ChartLibrary{}).rollbackRevision(config, request, false)
			}
			var refusal *Problem
			if !errors.As(err, &refusal) || refusal.Code != "target" || refusal.Detail != "Revision 2 changed after review." {
				t.Fatal("native operation accepted a copied installer label", err)
			}
			last, err := config.Releases.Last("oberth")
			if err != nil || last.Version != 2 || last.Info.Status != release.StatusPendingUpgrade {
				t.Fatal("refused native operation rewrote the pending revision", err)
			}
		})
	}
}

func stalePendingBinding(t *testing.T) (*action.Configuration, ChartRequest) {
	t.Helper()
	ch := &chart.Chart{Metadata: &chart.Metadata{Name: "oberth", APIVersion: "v2", Version: "1.0.0", AppVersion: "v0.1.0"}, Values: map[string]any{}}
	config := &action.Configuration{Releases: storage.Init(driver.NewMemory()), KubeClient: &kubefake.PrintingKubeClient{Out: io.Discard}, Log: func(string, ...any) {}}
	for i, status := range []release.Status{release.StatusSuperseded, release.StatusPendingUpgrade} {
		r := &release.Release{Name: "oberth", Namespace: "oberth", Version: i + 1, Chart: ch, Config: map[string]any{}, Info: &release.Info{Status: status}, Labels: revisionLabels(ChartRequest{Chart: ch})}
		if err := config.Releases.Create(r); err != nil {
			t.Fatal(err)
		}
	}
	h, err := readHistory(t.Context(), config, "oberth")
	if err != nil || h.Latest.Kind != "foreign" || h.Choices.Number != 1 {
		t.Fatal("fixture did not retain a stale pending binding", err)
	}
	r := ChartRequest{Chart: ch, Namespace: "oberth", Values: map[string]any{}, Recovery: &Recovery{Pending: 2}}
	r.bindHistory(h)
	return config, r
}
