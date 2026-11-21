package installer_test

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func allowForeignDeletion(kc *deltaDeletionKube) {
	kc.live = metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}
}

func foreignInstallFixture(t *testing.T, obsolete bool) (fixture, *storage.Storage, *deltaDeletionKube) {
	t.Helper()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	f.choices.Values = map[string]any{"runs": map[string]any{"parallel": 3}}
	apply(t, f, prepare(t, f))
	f.choices.Values = nil
	store, kc := delta2Native(t, f, false, obsolete)
	allowForeignDeletion(kc)
	return f, store, kc
}

func installerAttempt(t *testing.T, store *storage.Storage, status release.Status) int {
	t.Helper()
	first, err := store.Get("oberth", 1)
	if err != nil {
		t.Fatal(err)
	}
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	first.Version = last.Version + 1
	first.Info = &release.Info{Status: status}
	first.Manifest = ""
	first.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: first.Chart, Revision: first.Version - 1})
	if err = store.Create(first); err != nil {
		t.Fatal(err)
	}
	return first.Version
}

func TestForeignHistoryCannotBypassFailedFirstVersionGate(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(boolText(foreign), func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, false)
			if foreign {
				appendForeignRevisions(t, store)
			}
			first, err := store.Get("oberth", 1)
			if err != nil {
				t.Fatal(err)
			}
			first.Info.Status = release.StatusFailed
			if err = store.Update(first); err != nil {
				t.Fatal(err)
			}
			f.release = loadFixture(t, releaseFor(t, "v0.2.0", 2))
			before := deltaHistoryBytes(t, store)
			code := "upgrade-required"
			if foreign {
				code = "release-unowned"
			}
			assertInstallRefusal(t, f, code)
			if kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
				t.Fatal("version refusal changed native state")
			}
		})
	}
}

func TestForeignFailedAttemptKeepsNativeObsoleteDeletion(t *testing.T) {
	t.Parallel()
	for _, foreign := range []bool{false, true} {
		t.Run(boolText(foreign), func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, true)
			if foreign {
				appendForeignRevisions(t, store)
			}
			installerAttempt(t, store, release.StatusFailed)
			apply(t, f, prepare(t, f))
			if len(kc.deleted) != 1 || kc.updates != 1 {
				t.Fatal("failed manifest hid native obsolete object", kc.deleted, kc.updates)
			}
		})
	}
}

func TestForeignDeployedRevisionAllowsReviewedInstallerRepair(t *testing.T) {
	t.Parallel()
	for _, state := range []release.Status{release.StatusPendingUpgrade, release.StatusUninstalled} {
		for _, deployed := range []bool{false, true} {
			t.Run(string(state)+"/"+boolText(deployed), func(t *testing.T) { foreignRepairCase(t, state, deployed) })
		}
	}
}

func foreignRepairCase(t *testing.T, state release.Status, deployed bool) {
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	if !deployed {
		last, err := store.Last("oberth")
		if err != nil {
			t.Fatal(err)
		}
		last.Info.Status = release.StatusSuperseded
		if err = store.Update(last); err != nil {
			t.Fatal(err)
		}
	}
	installerAttempt(t, store, state)
	before := deltaHistoryBytes(t, store)
	plan := prepare(t, f)
	if !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("repair review changed history")
	}
	apply(t, f, plan)
	assertForeignApply(t, f, store, 5)
	if kc.updates != 1 {
		t.Fatal("reviewed repair did not update", kc.updates)
	}
}

func TestForeignRepairRejectsChangedDeployedRevision(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	installerAttempt(t, store, release.StatusPendingUpgrade)
	plan := prepare(t, f)
	for _, number := range []int{2, 3} {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Info.Status = release.StatusSuperseded
		if number == 2 {
			r.Info.Status = release.StatusDeployed
		}
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
	before := deltaHistoryBytes(t, store)
	_, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" || kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("changed deployed revision accepted", err)
	}
	newPlan := prepare(t, f)
	if plan.Digest() == newPlan.Digest() {
		t.Fatal("repair predecessor number omitted from approval")
	}
}

func TestForeignRevisionNumbersBindNewApproval(t *testing.T) {
	t.Parallel()
	f, store, kc := foreignInstallFixture(t, false)
	appendForeignRevisions(t, store)
	old := prepare(t, f)
	appendForeignRevisions(t, store)
	before := deltaHistoryBytes(t, store)
	_, err := f.engine.Apply(t.Context(), old, old.Digest(), nil)
	if err == nil {
		t.Fatal("original plan accepted changed history")
	}
	current := prepare(t, f)
	assertForeignReview(t, current, []int{2, 3, 4, 5})
	if current.Digest() == old.Digest() {
		t.Fatal("more foreign revisions reused approval digest")
	}
	_, err = f.engine.Apply(t.Context(), current, old.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "review" || kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("old approval authorized refreshed plan", err)
	}
	apply(t, f, current)
}

func TestForeignRemovalRemedyReusesNamespace(t *testing.T) {
	t.Parallel()
	for _, status := range []release.Status{release.StatusUninstalled, release.StatusUninstalling} {
		t.Run(string(status), func(t *testing.T) {
			f, store, kc := foreignInstallFixture(t, false)
			appendForeignRevisions(t, store)
			changeRevision(t, store, 3, func(record *release.Release) { record.Info.Status = status })
			unchanged := unchangedInstallation(t, f, store, kc)
			if status == release.StatusUninstalling {
				assertInstallRefusal(t, f, "foreign-removed")
				unchanged()
				// The outside tool finishes removal before the installer resumes.
				changeRevision(t, store, 3, func(record *release.Release) { record.Info.Status = release.StatusUninstalled })
				unchanged = unchangedInstallation(t, f, store, kc)
			}
			plan := prepare(t, f)
			defer plan.Close()
			if !strings.Contains(strings.Join(plan.Review(), "\n"), "Removed revision 3") {
				t.Fatal("completed removal review is unclear", plan.Review())
			}
			unchanged()
			apply(t, f, plan)
			assertForeignApply(t, f, store, 4)
			assertStoredParallel(t, store, 3)
		})
	}
}

func TestForeignRollbackRemedyCreatesUsableNextTransition(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(boolText(pending), func(t *testing.T) { foreignRemedyCase(t, pending) })
	}
}

func foreignRemedyCase(t *testing.T, pending bool) {
	t.Helper()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	makeForeignUpgradeRecord(t, f, m, pending)
	var err error
	numbers := appendForeignRevisions(t, store)
	if pending {
		changeForeignVersion(t, store, numbers, "v0.1.1")
		f.release = loadFixture(t, releaseFor(t, "v0.1.1", 1))
	}
	before := directoryBytes(t, f.dir)
	assertForeignRefusal(t, m.Rollback(t.Context()))
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) {
		t.Fatal("refusal changed operation record")
	}
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("advertised remedy left upgrade blocked", err)
	}
	defer next.Plan.Close()
	if strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("stale record selected as repair")
	}
	if _, err = m.Upgrade(t.Context(), next, next.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("new transition cannot roll back", err)
	}
}

func TestOrdinaryReapplySupersedesPendingUpgradeRecord(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	_, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.Endpoint {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if err == nil {
		t.Fatal("fixture did not interrupt")
	}
	apply(t, f, prepare(t, f))
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("ordinary reapply has a different stale-record rule", err)
	}
	defer next.Plan.Close()
	if strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("ordinary stale record selected as repair")
	}
}

func assertStoredParallel(t *testing.T, store *storage.Storage, want float64) {
	t.Helper()
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if last.Config["runs"].(map[string]any)["parallel"] != want {
		t.Fatal("failed or foreign values supplied choices")
	}
}

func makeForeignUpgradeRecord(t *testing.T, f fixture, m installer.Maintenance, pending bool) {
	t.Helper()
	first, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer first.Plan.Close()
	_, err = m.Upgrade(t.Context(), first, first.Plan.Digest(), func(phase installer.Phase) error {
		if pending && phase == installer.Endpoint {
			return io.ErrUnexpectedEOF
		}
		return nil
	})
	if (err != nil) != pending {
		t.Fatal("upgrade interruption fixture failed", err)
	}
}

func changeForeignVersion(t *testing.T, store *storage.Storage, numbers []int, version string) {
	t.Helper()
	for _, number := range numbers {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Chart.Metadata.AppVersion = version
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
}
