package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
)

func TestApprovedDeletionBaselineReachesNativeWrite(t *testing.T) {
	t.Parallel()
	for _, timing := range []string{"before-validation", "after-validation", "native-write", "before-repair"} {
		for _, shape := range []string{"manifest", "predecessor"} {
			t.Run(timing+"/"+shape, func(t *testing.T) {
				approvedDeletionWriteCase(t, timing, shape)
			})
		}
	}
}

func approvedDeletionWriteCase(t *testing.T, timing, shape string) {
	t.Helper()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	status := release.StatusFailed
	if timing == "before-repair" {
		status = release.StatusPendingUpgrade
	}
	installerAttempt(t, store, status)
	if shape == "manifest" {
		changeRevision(t, store, 3, func(r *release.Release) { r.Manifest = "" })
	} else {
		changeRevision(t, store, 1, func(r *release.Release) { r.Info.Status = release.StatusDeployed })
		changeRevision(t, store, 3, func(r *release.Release) { r.Info.Status = release.StatusSuperseded })
	}
	p := prepare(t, f)
	defer p.Close()
	var changed []byte
	change := func() {
		changeApprovedBaseline(t, store, shape)
		changed = deltaHistoryBytes(t, store)
	}
	if timing == "native-write" || timing == "before-repair" {
		f.engine.Charts = beforeChartApply{ChartPort: f.engine.Charts, check: func(installer.ChartRequest) {
			kc.reaches = 0
			kc.onReach = func() {
				if kc.reaches == map[string]int{"native-write": 4, "before-repair": 3}[timing] {
					change()
				}
			}
		}}
	}
	at := map[string]installer.Phase{"before-validation": installer.Checking, "after-validation": installer.ReleaseChecked}[timing]
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if phase == at {
			change()
		}
		return nil
	})
	if changed == nil {
		t.Fatal("baseline change did not reach its test boundary")
	}
	assertBaselineRefusal(t, err, shape)
	if kc.updates != 0 || len(kc.deleted) != 0 || !bytes.Equal(changed, deltaHistoryBytes(t, store)) {
		t.Fatal("unapproved baseline changed native resources or history", kc.updates, kc.deleted)
	}
}

func assertBaselineRefusal(t *testing.T, err error, shape string) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" {
		t.Fatal("changed deletion baseline reached the native write", err)
	}
	detail := "Revision 3 changed after review."
	if shape == "predecessor" {
		detail = "Predecessor changed after review: expected revision 1; found 2."
	}
	if refusal.Remedy != "Rerun the same oberth command to review the changed release." || refusal.Detail != detail {
		t.Fatal("baseline refusal omits the command or revision diagnostic", refusal)
	}
}

func changeApprovedBaseline(t *testing.T, store *storage.Storage, shape string) {
	t.Helper()
	if shape == "manifest" {
		changeRevision(t, store, 3, func(r *release.Release) { poisonHistoryTarget(r, "manifest") })
		return
	}
	changeRevision(t, store, 1, func(r *release.Release) { r.Info.Status = release.StatusSuperseded })
	changeRevision(t, store, 2, func(r *release.Release) { r.Info.Status = release.StatusDeployed })
}

func bindChartHistory(t *testing.T, port installer.ChartPort, request *installer.ChartRequest) {
	t.Helper()
	h, err := port.Read(t.Context(), request.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	baseline := h.Deployed
	if baseline.Number == 0 {
		baseline = h.Latest
	}
	request.Revision, request.Predecessor, request.ManifestDigest = h.Latest.Number, baseline.Number, baseline.ManifestDigest
}

func TestRemovedInstallerRevisionKeepsReviewedChoices(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"single", "upgraded", "failed"} {
		t.Run(shape, func(t *testing.T) { removedInstallerChoicesCase(t, shape) })
	}
}

func removedInstallerChoicesCase(t *testing.T, shape string) {
	t.Helper()
	f, store, _ := foreignInstallFixture(t, false)
	changeRevision(t, store, 1, func(r *release.Release) {
		r.Config["database"] = map[string]any{"file": "/var/lib/oberth/custom.db"}
	})
	want := 3
	if shape != "single" {
		want = 5
		f.choices.Parallel = &want
		apply(t, f, prepare(t, f))
		f.choices.Parallel = nil
	}
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if shape == "failed" {
		changeRevision(t, store, last.Version, func(r *release.Release) { r.Info.Status = release.StatusFailed })
	}
	changeRevision(t, store, last.Version, func(r *release.Release) { r.Info.Status = release.StatusUninstalled })
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Deployed.Number != 0 || h.Choices.Number != last.Version {
		t.Fatal("removed installer revision did not retain its choices", h.Choices.Number, last.Version, err)
	}
	p := prepare(t, f)
	defer p.Close()
	text := strings.Join(p.Review(), "\n")
	if !strings.Contains(text, fmt.Sprintf("Saved choices: revision %d (uninstalled)", last.Version)) || !strings.Contains(text, fmt.Sprintf("Parallel runs: %d ", want)) || !strings.Contains(strings.Join(p.Detail(), "\n"), "/var/lib/oberth/custom.db") {
		t.Fatal("review does not show the removed revision's choices", text)
	}
	apply(t, f, p)
	assertStoredParallel(t, store, float64(want))
	last, err = store.Last("oberth")
	if err != nil || last.Config["database"].(map[string]any)["file"] != "/var/lib/oberth/custom.db" {
		t.Fatal("removal reset the reviewed database path", err)
	}
}

func TestInterruptedUpgradeOvertakenByFailedOutsideUpgrade(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	target := f.release
	f.release = loadFixture(t, releaseFor(t, "v0.0.9", 1))
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	f.release = target
	makeForeignUpgradeRecord(t, f, m, true)
	failedOutsideUpgrade(t, store, "v0.1.1")
	unchanged := unchangedInstallation(t, f, store, kc)
	assertForeignRefusal(t, m.Rollback(t.Context()))
	assertInstallRefusal(t, f, "upgrade-required")
	f.release = loadFixture(t, releaseFor(t, "v0.1.1", 1))
	assertInstallRefusal(t, f, "upgrade-required")
	unchanged()
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("failed outside upgrade left no reviewed recovery route", err)
	}
	defer p.Plan.Close()
	if strings.Contains(strings.Join(p.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("overtaken transition reused its stale repair record")
	}
	result, err := m.Upgrade(t.Context(), p, p.Plan.Digest(), nil)
	if err != nil || result.Upgrade.From != "v0.1.0" || result.Upgrade.Revision != 4 {
		t.Fatal("new upgrade did not start at the running revision", result.Upgrade, err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("new transition did not create a usable rollback point", err)
	}
	last, err := store.Last("oberth")
	if err != nil || last.Chart.Metadata.AppVersion != "v0.1.0" {
		t.Fatal("new rollback did not restore the running version", err)
	}
}

func failedOutsideUpgrade(t *testing.T, store *storage.Storage, version string) {
	t.Helper()
	number := installerAttempt(t, store, release.StatusFailed)
	changeRevision(t, store, number, func(r *release.Release) {
		r.Chart.Metadata.AppVersion = version
		r.Labels["oberth.ci/kind-revision"] = "copied"
	})
}

func TestOutsideRollbackRemedyWorksWithFailedVersionMismatch(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	makeForeignUpgradeRecord(t, f, m, false)
	failedOutsideUpgrade(t, store, "v0.1.0")
	unchanged := unchangedInstallation(t, f, store, kc)
	assertForeignRefusal(t, m.Rollback(t.Context()))
	assertInstallRefusal(t, f, "upgrade-required")
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	assertInstallRefusal(t, f, "upgrade-required")
	unchanged()
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("advertised upgrade remedy was refused", err)
	}
	defer p.Plan.Close()
	result, err := m.Upgrade(t.Context(), p, p.Plan.Digest(), nil)
	if err != nil || result.Upgrade.From != "v0.2.0" || result.Upgrade.To != "v0.2.0" || result.Upgrade.Revision != 4 {
		t.Fatal("reviewed remedy failed to create a same-version transition", result.Upgrade, err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("advertised remedy created an unusable rollback point", err)
	}
}

func TestRecordRetainsOneRevisionClassification(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		installer bool
	}{{"local", true}, {"verified", true}, {"foreign", false}, {"", false}, {"unknown", false}} {
		t.Run(tc.kind, func(t *testing.T) {
			raw, err := json.Marshal(installer.Record{Source: installer.Revision{Kind: tc.kind}})
			if err != nil || bytes.Contains(raw, []byte(`"Installer"`)) {
				t.Fatal("record persisted a second revision classification", err)
			}
			var record installer.Record
			if err = json.Unmarshal(raw, &record); err != nil || record.Source.Installer() != tc.installer {
				t.Fatal("record lost its revision classification", tc.kind, err)
			}
		})
	}
}
