package installer_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
)

type refusingChart struct {
	installer.ChartPort
	phase string
	err   error
}

func (c refusingChart) Read(ctx context.Context, ns string) (installer.History, error) {
	if c.phase == "read" {
		return installer.History{}, c.err
	}
	return c.ChartPort.Read(ctx, ns)
}
func (c refusingChart) Preview(ctx context.Context, r installer.ChartRequest) error {
	if c.phase == "preview" {
		return c.err
	}
	return c.ChartPort.Preview(ctx, r)
}
func (c refusingChart) Apply(ctx context.Context, r installer.ChartRequest) error {
	if c.phase == "apply" {
		return c.err
	}
	return c.ChartPort.Apply(ctx, r)
}

func TestChartRefusalKeepsItsOwnReason(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"history-delete", "release-provenance"} {
		for _, phase := range []string{"read", "preview", "apply"} {
			t.Run(code+"/"+phase, func(t *testing.T) {
				f := setup(t)
				cause := errors.New("native diagnostic")
				want := installer.NewProblem(code, cause, "ConfigMap oberth/owned")
				f.engine.Charts = refusingChart{f.charts, phase, fmt.Errorf("native: %w", want)}
				plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
				if phase == "apply" {
					if err != nil {
						t.Fatal(err)
					}
					defer plan.Close()
					_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
				}
				if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), want.Reason) || !errors.Is(err, cause) {
					t.Fatal("structured chart refusal was masked", err)
				}
			})
		}
	}
}

func TestUnknownChartErrorRetainsGenericGuidance(t *testing.T) {
	t.Parallel()
	f := setup(t)
	cause := errors.New("native dry run unavailable")
	f.engine.Charts = refusingChart{f.charts, "preview", cause}
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	var p *installer.Problem
	if !errors.As(err, &p) || p.Code != "chart" || !errors.Is(err, cause) || !strings.Contains(p.Detail, cause.Error()) {
		t.Fatal("unknown native error lost its guidance or diagnostic", err)
	}
}

func TestRevalidationChartRefusalKeepsItsOwnReason(t *testing.T) {
	t.Parallel()
	for _, code := range []string{"history-delete", "release-provenance"} {
		t.Run(code, func(t *testing.T) {
			f := setup(t)
			plan := prepare(t, f)
			want := installer.NewProblem(code, nil, "ConfigMap oberth/owned")
			f.engine.Charts = refusingChart{f.charts, "preview", fmt.Errorf("native: %w", want)}
			actions := len(f.client.Actions())
			_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
			if !errors.Is(err, want) || !strings.HasPrefix(err.Error(), want.Reason) {
				t.Fatal("revalidation masked structured chart refusal", err)
			}
			deltaAssertOnlyClusterReads(t, f, actions)
		})
	}
}

func TestHistoryRemediesNameRecoverableState(t *testing.T) {
	t.Parallel()
	for code, words := range map[string][]string{"release-provenance": {"Helm release history", "data volume", "storage level", "by hand"}, "history-delete": {"namespace", "ownership", "history", "review"}} {
		remedy := installer.NewProblem(code, nil, "").Remedy
		if strings.Contains(strings.ToLower(remedy), "backup") || len(remedy) > 120 {
			t.Fatal("history remedy claims unsupported backup recovery", code, remedy)
		}
		for _, word := range words {
			if !strings.Contains(remedy, word) {
				t.Fatal("history remedy omits its actual recovery option", code, word)
			}
		}
	}
}

func TestNativeDeletionAlreadyGoneDoesNotBlockUpgrade(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(boolText(foreign), func(t *testing.T) { deletionReadCase(t, http.StatusNotFound, foreign) })
	}
}

func TestNativeDeletionReadErrorsStillRefuse(t *testing.T) {
	t.Parallel()
	for _, code := range []int{http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) { deletionReadCase(t, code, false) })
	}
}

func deletionReadCase(t *testing.T, status int, foreign bool) {
	t.Helper()
	store, request := reviewHistoryFixture(t)
	request.Revision, request.Recovery, request.RollbackID = 3, nil, "reviewed"
	old, err := store.Get("oberth", 2)
	if err != nil {
		t.Fatal(err)
	}
	poisonHistoryTarget(old, "both")
	if err = store.Update(old); err != nil {
		t.Fatal(err)
	}
	request.Chart = installer.ReleaseChartForTest(loadFixture(t, releaseFixture(t)))
	kc := deltaDeleteClient(t)
	kc.getStatus = status
	if foreign {
		kc.info.Namespace = "foreign"
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	port := installer.ChartForTest(&action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}})
	bindChartHistory(t, port, &request)
	before := deltaHistoryBytes(t, store)
	err = port.Preview(t.Context(), request)
	if !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("preview changed history")
	}
	if status != http.StatusNotFound {
		var refusal *installer.Problem
		if !errors.As(err, &refusal) || refusal.Code != "history-delete" || kc.updates != 0 {
			t.Fatal("unreadable deletion candidate was accepted", err)
		}
		return
	}
	if err != nil {
		t.Fatal("already absent object blocks preview", err)
	}
	if err = port.Apply(t.Context(), request); err != nil {
		t.Fatal("already absent object blocks upgrade", err)
	}
	assertAbsentDeletionCompleted(t, store, kc)
}

func assertAbsentDeletionCompleted(t *testing.T, store *storage.Storage, kc *deltaDeletionKube) {
	t.Helper()
	latest, err := store.Last("oberth")
	if err != nil || latest.Version != 4 || latest.Info.Status != release.StatusDeployed || kc.updates != 1 || len(kc.deleted) != 0 {
		t.Fatal("native upgrade did not complete the absent-object deletion", err)
	}
}
