package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"slices"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func TestRemovedRepairCanRetryAfterNativeCreateStops(t *testing.T) {
	t.Parallel()
	for _, stop := range []string{"api-create", "write-guard"} {
		for _, route := range []struct{ command, version string }{{"install", "v0.1.0"}, {"upgrade", "v0.1.0"}, {"upgrade", "v0.2.0"}} {
			t.Run(stop+"/"+route.command+"/"+route.version, func(t *testing.T) {
				removedRepairStopCase(t, stop, route.command, route.version)
			})
		}
	}
}

func removedRepairStopCase(t *testing.T, stop, command, version string) {
	t.Helper()
	f, store, kc, oldApproval := stoppedRemovalFixture(t, stop)
	unchanged := unchangedInstallation(t, f, store, kc)
	f.release = loadFixture(t, releaseFor(t, version, 1))
	if version != "v0.1.0" {
		assertInstallRefusal(t, f, "upgrade-required")
		unchanged()
	}
	next, run := stoppedRemovalRetry(t, f, command)
	defer next.Close()
	if !slices.Contains(next.Review(), "Saved choices: revision 1 (superseded)") {
		t.Fatal("retry hid its saved-choice source", next.Review())
	}
	unchanged()
	for _, approval := range []string{"", oldApproval} {
		var refusal *installer.Problem
		if _, err := run(approval); !errors.As(err, &refusal) || refusal.Code != "review" {
			t.Fatal("retry did not require its new review", err)
		}
		unchanged()
	}
	result, err := run(next.Digest())
	if err != nil {
		t.Fatal("approved retry failed", err)
	}
	assertStoppedRetryApplied(t, store, kc, version)
	if command == "upgrade" {
		assertSupersededUpgradeSource(t, f, result)
		assertInstallAfterRemovedUpgrade(t, f, store, kc)
	}
}

func stoppedRemovalFixture(t *testing.T, stop string) (fixture, *storage.Storage, *deltaDeletionKube, string) {
	t.Helper()
	f, store, kc := foreignInstallFixture(t, false)
	changeRevision(t, store, 1, func(r *release.Release) { r.Info.Status = release.StatusUninstalled })
	p := prepare(t, f)
	defer p.Close()
	restore := stopRemovedRevision(t, store, kc, stop)
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	restore()
	var refusal *installer.Problem
	want := "chart"
	if stop == "write-guard" {
		want = "target"
	}
	if !errors.As(err, &refusal) || refusal.Code != want {
		t.Fatal("native upgrade did not stop at the intended boundary", err)
	}
	assertStoppedRemoval(t, f, kc)
	return f, store, kc, p.Digest()
}

func assertStoppedRetryApplied(t *testing.T, store *storage.Storage, kc *deltaDeletionKube, version string) {
	t.Helper()
	last, err := store.Last("oberth")
	if err != nil || last.Version != 2 || last.Info.Status != release.StatusDeployed || last.Chart.Metadata.AppVersion != version || kc.updates != 1 {
		t.Fatal("reviewed retry did not deploy revision 2", err)
	}
	assertStoredParallel(t, store, 3)
}

func stoppedRemovalRetry(t *testing.T, f fixture, command string) (*installer.Plan, func(string) (installer.Result, error)) {
	t.Helper()
	if command == "install" {
		p := prepare(t, f)
		return p, func(approval string) (installer.Result, error) { return f.engine.Apply(t.Context(), p, approval, nil) }
	}
	m := removedMaintenance(t, f)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("stopped removal has no reviewed upgrade route", err)
	}
	if !slices.Contains(p.Plan.Review(), "Release: v0.1.0 -> "+f.release.Version()) {
		t.Fatal("upgrade review lost its source version", p.Plan.Review())
	}
	return p.Plan, func(approval string) (installer.Result, error) { return m.Upgrade(t.Context(), p, approval, nil) }
}

func removedMaintenance(t *testing.T, f fixture) installer.Maintenance {
	t.Helper()
	_, options := backupFixture(t)
	return installer.Maintenance{Engine: f.engine, Options: options, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }}
}

func assertSupersededUpgradeSource(t *testing.T, f fixture, result installer.Result) {
	t.Helper()
	if result.Upgrade.From != "v0.1.0" || result.Upgrade.To != f.release.Version() || result.Upgrade.Revision != 2 {
		t.Fatal("upgrade completion lost its superseded source", result.Upgrade)
	}
	raw, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	runs, _ := record.Source.Values["runs"].(map[string]any)
	if record.Source.Number != 1 || record.Source.Version != "v0.1.0" || record.Source.Status != "superseded" || !record.Source.Installer() || runs["parallel"] != float64(3) {
		t.Fatal("rollback record lost its superseded source", record.Source)
	}
}

func assertInstallAfterRemovedUpgrade(t *testing.T, f fixture, store *storage.Storage, kc *deltaDeletionKube) {
	t.Helper()
	apply(t, f, prepare(t, f))
	last, err := store.Last("oberth")
	if err != nil || last.Version != 3 || last.Info.Status != release.StatusDeployed || last.Chart.Metadata.AppVersion != f.release.Version() || kc.updates != 2 {
		t.Fatal("install did not reapply the upgraded version", err)
	}
	assertStoredParallel(t, store, 3)
}

func assertStoppedRemoval(t *testing.T, f fixture, kc *deltaDeletionKube) {
	t.Helper()
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Latest.Number != 1 || h.Latest.Status != "superseded" || h.Deployed.Number != 0 || h.Choices.Number != 1 || kc.updates != 0 {
		t.Fatal("stopped native upgrade did not leave the removed repair state", h, err)
	}
}

func stopRemovedRevision(t *testing.T, store *storage.Storage, kc *deltaDeletionKube, stop string) func() {
	t.Helper()
	original := store.Driver
	if stop == "api-create" {
		store.Driver = stoppedReleaseCreate{Driver: original}
	} else {
		kc.reaches = 0
		kc.onReach = func() {
			last, err := store.Last("oberth")
			if err != nil {
				t.Fatal(err)
			}
			if last.Info.Status == release.StatusSuperseded {
				changeRevision(t, store, 1, func(r *release.Release) { r.Manifest += "\n# changed baseline\n" })
				kc.onReach = nil
			}
		}
	}
	return func() { store.Driver, kc.onReach = original, nil }
}

type stoppedReleaseCreate struct{ driver.Driver }

func (d stoppedReleaseCreate) Create(key string, record *release.Release) error {
	if record.Version == 2 {
		return errors.New("release API unavailable at create")
	}
	return d.Driver.Create(key, record)
}

func TestSupersededLatestWithDeployedRevisionStillRefuses(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	installerAttempt(t, store, release.StatusSuperseded)
	unchanged := unchangedInstallation(t, f, store, kc)
	assertInstallRefusal(t, f, "chart-state")
	for _, version := range []string{"v0.1.0", "v0.2.0"} {
		f.release = loadFixture(t, releaseFor(t, version, 1))
		assertRemovedUpgradeRefusal(t, f, "chart-state")
	}
	unchanged()
}

func TestUpgradeWithoutDeployedSourceRejectsOtherStates(t *testing.T) {
	t.Parallel()
	for _, status := range []release.Status{release.StatusFailed, release.StatusPendingUpgrade, release.StatusUninstalled} {
		t.Run(string(status), func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, false)
			changeRevision(t, store, 1, func(r *release.Release) { r.Info.Status = status })
			unchanged := unchangedInstallation(t, f, store, kc)
			assertRemovedUpgradeRefusal(t, f, "upgrade-source")
			unchanged()
		})
	}
}

func assertRemovedUpgradeRefusal(t *testing.T, f fixture, code string) {
	t.Helper()
	m := removedMaintenance(t, f)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if p != nil {
		defer p.Plan.Close()
	}
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatal("unsupported upgrade source accepted", err)
	}
}
