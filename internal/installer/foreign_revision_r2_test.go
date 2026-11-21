package installer_test

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
)

func changeRevision(t *testing.T, store *storage.Storage, number int, change func(*release.Release)) {
	t.Helper()
	record, err := store.Get("oberth", number)
	if err != nil {
		t.Fatal(err)
	}
	change(record)
	if err = store.Update(record); err != nil {
		t.Fatal(err)
	}
}

func TestFailedLatestCannotHideRunningVersion(t *testing.T) {
	t.Parallel()
	for _, outside := range []bool{false, true} {
		for _, state := range []release.Status{release.StatusFailed, release.StatusPendingUpgrade} {
			t.Run(boolText(outside)+"/"+string(state), func(t *testing.T) {
				f := setup(t)
				f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
				apply(t, f, prepare(t, f))
				store, kc := delta2Native(t, f, false, false)
				allowForeignDeletion(kc)
				if outside {
					appendForeignRevisions(t, store)
				}
				f.release = loadFixture(t, releaseFixture(t))
				number := installerAttempt(t, store, state)
				changeRevision(t, store, number, func(record *release.Release) {
					record.Chart = installer.ReleaseChartForTest(f.release)
					record.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: record.Chart, Revision: number - 1})
				})
				unchanged := unchangedInstallation(t, f, store, kc)
				assertInstallRefusal(t, f, "upgrade-required")
				unchanged()
			})
		}
	}
}

func TestPartialPruningNeverDefaultsAppliedSettings(t *testing.T) {
	t.Parallel()
	for _, state := range []release.Status{release.StatusFailed, release.StatusPendingUpgrade} {
		t.Run(string(state), func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, false)
			changeRevision(t, store, 1, func(record *release.Release) {
				record.Config["database"] = map[string]any{"file": "/var/lib/oberth/custom.db"}
			})
			appendForeignRevisions(t, store)
			installerAttempt(t, store, state)
			if _, err := store.Delete("oberth", 1); err != nil {
				t.Fatal(err)
			}
			unchanged := unchangedInstallation(t, f, store, kc)
			p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if p != nil {
				p.Close()
			}
			var refusal *installer.Problem
			if !errors.As(err, &refusal) || refusal.Code != "release-unowned" || !strings.Contains(refusal.Reason, "Applied installer settings") || !strings.Contains(refusal.Remedy, "repair the saved settings by hand") {
				t.Fatal("missing choices have no explicit recovery instruction", err)
			}
			unchanged()
		})
	}
}

func TestApprovalBindsNativeDeletionFacts(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"manifest", "predecessor"} {
		t.Run(shape, func(t *testing.T) {
			nativeDeletionApprovalCase(t, shape)
		})
	}
}

func nativeDeletionApprovalCase(t *testing.T, shape string) {
	t.Helper()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	installerAttempt(t, store, release.StatusFailed)
	if shape == "manifest" {
		changeRevision(t, store, 3, func(record *release.Release) { record.Manifest = "" })
	} else {
		changeRevision(t, store, 1, func(record *release.Release) { record.Info.Status = release.StatusDeployed })
		changeRevision(t, store, 3, func(record *release.Release) { record.Info.Status = release.StatusSuperseded })
	}
	old := prepare(t, f)
	defer old.Close()
	if shape == "manifest" {
		changeRevision(t, store, 3, func(record *release.Release) { poisonHistoryTarget(record, "both") })
	} else {
		changeRevision(t, store, 1, func(record *release.Release) { record.Info.Status = release.StatusSuperseded })
		changeRevision(t, store, 2, func(record *release.Release) { record.Info.Status = release.StatusDeployed })
	}
	unchanged := unchangedInstallation(t, f, store, kc)
	var refusal *installer.Problem
	if _, err := f.engine.Apply(t.Context(), old, old.Digest(), nil); !errors.As(err, &refusal) || refusal.Code != "target" {
		t.Fatal("original plan accepted changed native deletion facts", err)
	}
	fresh := prepare(t, f)
	defer fresh.Close()
	if fresh.Digest() == old.Digest() {
		t.Fatal("changed native deletion facts reused approval")
	}
	if _, err := f.engine.Apply(t.Context(), fresh, old.Digest(), nil); !errors.As(err, &refusal) || refusal.Code != "review" {
		t.Fatal("old approval authorized changed native deletion facts", err)
	}
	unchanged()
	apply(t, f, fresh)
	if kc.updates != 1 || len(kc.deleted) != 1 {
		t.Fatal("new approval did not authorize the reviewed native deletion", kc.updates, kc.deleted)
	}
	assertStoredParallel(t, store, 3)
}

func TestOrdinaryApplyClearsNativeRollbackLineage(t *testing.T) {
	t.Parallel()
	for _, interrupted := range []bool{false, true} {
		t.Run(boolText(interrupted), func(t *testing.T) {
			ordinaryApplyAfterRollbackCase(t, interrupted)
		})
	}
}

func ordinaryApplyAfterRollbackCase(t *testing.T, interrupted bool) {
	t.Helper()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	appendForeignRevisions(t, store)
	makeForeignUpgradeRecord(t, f, m, interrupted)
	endpoint := f.engine.CheckEndpoint
	var want error
	if interrupted {
		want = io.ErrUnexpectedEOF
		f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return io.ErrUnexpectedEOF }
	}
	err := m.Rollback(t.Context())
	if !errors.Is(err, want) {
		t.Fatal("rollback fixture did not reach its endpoint", err)
	}
	f.engine.CheckEndpoint = endpoint
	f.choices.Parallel = new(int)
	*f.choices.Parallel = 7
	apply(t, f, prepare(t, f))
	last, err := store.Last("oberth")
	if err != nil || last.Labels["oberth.ci/rollback"] != "" {
		t.Fatal("ordinary native action inherited rollback identity", err)
	}
	unchanged := unchangedInstallation(t, f, store, kc)
	var refusal *installer.Problem
	if err = m.Rollback(t.Context()); !errors.As(err, &refusal) || refusal.Code != "rollback-superseded" {
		t.Fatal("ordinary apply kept the superseded rollback point", err)
	}
	unchanged()
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Plan.Close()
	if strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("ordinary apply left a stale repair operation")
	}
	if _, err = m.Upgrade(t.Context(), next, next.Plan.Digest(), nil); err != nil {
		t.Fatal("new transition remained blocked by old rollback", err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("new rollback point was unusable", err)
	}
	assertStoredParallel(t, store, 7)
}

func TestInvalidRollbackRecordOffersIntactRecovery(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	if err := os.WriteFile(f.dir+"/rollback.json", []byte("{"), 0600); err != nil {
		t.Fatal(err)
	}
	unchanged := unchangedInstallation(t, f, store, kc)
	for _, operation := range []func() error{
		func() error { return m.Rollback(t.Context()) },
		func() error {
			p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
			if p != nil {
				p.Plan.Close()
			}
			return err
		},
	} {
		var refusal *installer.Problem
		if err := operation(); !errors.As(err, &refusal) || refusal.Code != "rollback" || strings.Contains(refusal.Remedy, "oberth upgrade") || !strings.Contains(refusal.Remedy, "intact installation profile") {
			t.Fatal("invalid record offered an upgrade it prevents", err)
		}
		unchanged()
	}
}

func TestFailedForeignReviewNamesInstallerChoices(t *testing.T) {
	t.Parallel()
	f, store, _ := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	installerAttempt(t, store, release.StatusFailed)
	p := prepare(t, f)
	defer p.Close()
	if !strings.Contains(strings.Join(p.Review(), "\n"), "use installer choices") {
		t.Fatal("review does not name the actual values source", p.Review())
	}
	apply(t, f, p)
	assertStoredParallel(t, store, 3)
}
