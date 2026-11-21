package installer_test

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
)

// Check the native release, cluster objects and local credentials together.
func unchangedInstallation(t *testing.T, f fixture, store *storage.Storage, kc *deltaDeletionKube) func() {
	t.Helper()
	history, files := deltaHistoryBytes(t, store), directoryBytes(t, f.dir)
	actions, updates, deletes := len(f.client.Actions()), kc.updates, len(kc.deleted)
	return func() {
		t.Helper()
		deltaAssertOnlyClusterReads(t, f, actions)
		if !bytes.Equal(history, deltaHistoryBytes(t, store)) || !reflect.DeepEqual(files, directoryBytes(t, f.dir)) || kc.updates != updates || len(kc.deleted) != deletes {
			t.Fatal("refusal changed the installation")
		}
	}
}

func TestForeignFailedLatestCannotDowngradeRunningVersion(t *testing.T) {
	t.Parallel()
	for _, running := range []string{"v0.1.0", "v0.2.0"} {
		t.Run(running, func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, false)
			changeRevision(t, store, 1, func(record *release.Release) {
				record.Chart.Metadata.AppVersion = running
				record.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: record.Chart})
			})
			number := installerAttempt(t, store, release.StatusFailed)
			changeRevision(t, store, number, func(record *release.Release) {
				record.Chart.Metadata.AppVersion = "v0.1.0"
				record.Labels["oberth.ci/kind-revision"] = "copied"
			})
			unchanged := unchangedInstallation(t, f, store, kc)
			plan, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			if plan != nil {
				defer plan.Close()
			}
			if running == "v0.1.0" {
				if err != nil {
					t.Fatal("matching running version was refused", err)
				}
			} else {
				var refusal *installer.Problem
				if !errors.As(err, &refusal) || refusal.Code != "upgrade-required" || !strings.Contains(refusal.Detail, "Revision 1: v0.2.0 (deployed)") {
					t.Fatal("failed outside attempt hid the running version", err)
				}
			}
			unchanged()
		})
	}
}

func TestSupersededRollbackRemedyReturnsEarlierVersion(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	previous := f.release
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	makeForeignUpgradeRecord(t, f, m, false)
	f.choices.Parallel = new(int)
	*f.choices.Parallel = 7
	apply(t, f, prepare(t, f))
	unchanged := unchangedInstallation(t, f, store, kc)
	var refusal *installer.Problem
	if err := m.Rollback(t.Context()); !errors.As(err, &refusal) || refusal.Code != "rollback-superseded" || !strings.Contains(refusal.Remedy, "oberth upgrade --version <earlier-version>") {
		t.Fatal("superseded rollback did not explain the way back", err)
	}
	unchanged()
	plan, err := m.PlanUpgrade(t.Context(), previous, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("earlier-version remedy was refused", err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal("earlier-version remedy failed", err)
	}
	last, err := store.Last("oberth")
	if err != nil || last.Chart.Metadata.AppVersion != previous.Version() {
		t.Fatal("remedy did not return to the earlier version", err)
	}
	assertStoredParallel(t, store, 7)
}
