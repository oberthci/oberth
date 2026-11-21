package installer_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
)

func TestForeignHistorySelectsNewestAppliedInstallerChoices(t *testing.T) {
	t.Parallel()
	f, store, _ := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	number := installerAttempt(t, store, release.StatusFailed)
	failed, err := store.Get("oberth", number)
	if err != nil {
		t.Fatal(err)
	}
	failed.Config = map[string]any{"runs": map[string]any{"parallel": 55}}
	if err = store.Update(failed); err != nil {
		t.Fatal(err)
	}
	appendForeignRevisions(t, store)
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Latest.Number != 6 || h.Deployed.Number != 6 || h.Choices.Number != 1 || h.Deployed.Values != nil {
		t.Fatal("native facts and stored choices were conflated", h, err)
	}
	apply(t, f, prepare(t, f))
	assertStoredParallel(t, store, 3)
	appendForeignRevisions(t, store)
	h, err = f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Choices.Number != 7 || h.Latest.Number != 9 || h.Deployed.Number != 9 {
		t.Fatal("newer installer choices lost", h, err)
	}
}

func TestForeignVersionsRemainNativeFactsWithoutForeignValues(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"v0.0.1", "v9.9.9"} {
		t.Run(version, func(t *testing.T) { foreignVersionCase(t, version) })
	}
}

func foreignVersionCase(t *testing.T, version string) {
	f, store, _ := foreignInstallFixture(t, false)
	for _, number := range appendForeignRevisions(t, store) {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Config = map[string]any{}
		r.Chart.Metadata.AppVersion = version
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
	assertInstallRefusal(t, f, "upgrade-required")
	_, options := backupFixture(t)
	m := installer.Maintenance{Engine: f.engine, Options: options, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }}
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	text := strings.Join(p.Plan.Review(), "\n")
	if !strings.Contains(text, "Release: "+version+" -> "+f.release.Version()) || !strings.Contains(text, "Parallel runs: 3 ") {
		t.Fatal("review lost native version or installer choices", text)
	}
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	assertStoredParallel(t, store, 3)
}

func TestForeignBeforeTargetAllowsRollbackBeforeChartChange(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	appendForeignRevisions(t, store)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	_, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.Checking {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal("fixture did not interrupt before chart change", err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("unchanged reviewed foreign source cannot finish rollback", err)
	}
}

func TestForeignShapesNeverHideVerifiedInstallerHistory(t *testing.T) {
	t.Parallel()
	for _, foreign := range [][]int{{1}, {3}, {1, 3}} {
		for _, claimed := range []string{"", "local", "verified"} {
			t.Run(jsonOf(t, foreign)+"/"+claimed, func(t *testing.T) { verifiedForeignCase(t, foreign, claimed) })
		}
	}
}
func verifiedForeignCase(t *testing.T, foreign []int, claimed string) {
	port, store, request := pendingFixture(t, true)
	for number := 1; number <= 3; number++ {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Info.Status = release.StatusSuperseded
		if number == 3 {
			r.Info.Status = release.StatusDeployed
		}
		if slices.Contains(foreign, number) {
			r.Labels["oberth.ci/release-kind"] = claimed
			r.Labels["oberth.ci/kind-revision"] = "copied"
			r.Labels["oberth.ci/rollback"] = "copied"
			r.Config = map[string]any{"foreign-only": "ignored"}
		}
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
	h, err := port.Read(t.Context(), "oberth")
	if err != nil {
		t.Fatal(err)
	}
	assertForeignClassification(t, h)
	bindChartHistory(t, port, &request)
	request.Local, request.Recovery = true, nil
	var refusal *installer.Problem
	if err = port.Preview(t.Context(), request); !errors.As(err, &refusal) || refusal.Code != "local-release" {
		t.Fatal("verified history accepted local chart", err)
	}
}
func assertForeignClassification(t *testing.T, h installer.History) {
	t.Helper()
	if !h.Verified {
		t.Fatal("foreign claims hid verified history")
	}
	if h.Latest.Kind == "foreign" && (h.Latest.Values != nil || h.Latest.RollbackID != "") {
		t.Fatal("foreign claims gained installer authority")
	}
}
