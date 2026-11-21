package installer_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const foreignReviewLine = "Helm revision %d: changed outside Oberth; use reviewed values."

// Copy revision labels just as an external upgrade and rollback do. Neither
// revision binds its copied labels to its new number.
func appendForeignRevisions(t *testing.T, store *storage.Storage) []int {
	t.Helper()
	original, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	deployed, err := store.Deployed("oberth")
	if err == nil {
		deployed.Info.Status = release.StatusSuperseded
		if err = store.Update(deployed); err != nil {
			t.Fatal(err)
		}
	}
	var numbers []int
	for offset := 1; offset <= 2; offset++ {
		foreign := *original
		foreign.Version += offset
		foreign.Info = &release.Info{Status: release.StatusSuperseded}
		if offset == 2 {
			foreign.Info.Status = release.StatusDeployed
		}
		foreign.Config = map[string]any{"runs": map[string]any{"parallel": 9876}, "foreign-only": "ignored"}
		poisonHistoryTarget(&foreign, "both")
		if err = store.Create(&foreign); err != nil {
			t.Fatal(err)
		}
		numbers = append(numbers, foreign.Version)
	}
	return numbers
}

func TestForeignRevisionInstallReapplyReviewsAndBinds(t *testing.T) {
	t.Parallel()
	for _, local := range []bool{true, false} {
		t.Run(boolText(local), func(t *testing.T) {
			f := setup(t)
			if !local {
				f.release = loadFixture(t, releaseFixture(t))
			}
			apply(t, f, prepare(t, f))
			store, kc := delta2Native(t, f, false, false)
			kc.live = metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}
			original, err := store.Last("oberth")
			if err != nil {
				t.Fatal(err)
			}
			original.Labels = installer.RevisionLabelsForTest(installer.ChartRequest{Chart: original.Chart, Local: local})
			if err = store.Update(original); err != nil {
				t.Fatal(err)
			}
			clean := prepare(t, f)
			numbers := appendForeignRevisions(t, store)
			before := deltaHistoryBytes(t, store)
			plan := prepare(t, f)
			assertForeignReview(t, plan, numbers)
			if plan.Digest() == clean.Digest() || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
				t.Fatal("foreign review was unbound or wrote history")
			}
			apply(t, f, plan)
			assertForeignApply(t, f, store, numbers[1]+1)
			if len(kc.deleted) != 1 || kc.updates != 1 {
				t.Fatal("native apply did not remove the release-owned foreign object", kc.deleted, kc.updates)
			}
			assertForeignReview(t, prepare(t, f), numbers)
		})
	}
}

func assertForeignReview(t *testing.T, p *installer.Plan, numbers []int) {
	t.Helper()
	count := 0
	want := fmt.Sprintf(foreignReviewLine, numbers[len(numbers)-1])
	for _, line := range p.Review() {
		if line == want {
			count++
		}
	}
	if count != 1 || len(want) > 80 {
		t.Fatal("foreign review must contain one complete short line", p.Review())
	}
	var detail struct {
		Foreign []int `json:"foreign_revisions"`
	}
	text := strings.Join(p.Detail(), "\n")
	if err := json.Unmarshal([]byte(text), &detail); err != nil || !slices.Equal(detail.Foreign, numbers) || strings.Contains(text, "foreign-only") {
		t.Fatal("detail lost revision numbers or read foreign values", text, err)
	}
}

func assertForeignApply(t *testing.T, f fixture, store *storage.Storage, number int) {
	t.Helper()
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	if last.Version != number || last.Labels["oberth.ci/kind-revision"] != installer.RevisionBindingForTest(number, last.Labels["oberth.ci/release-kind"], f.release.Version()) {
		t.Fatal("new installer revision was not bound", last.Version, last.Labels)
	}
	if _, ok := last.Config["foreign-only"]; ok || last.Config["runs"].(map[string]any)["parallel"] == float64(9876) {
		t.Fatal("apply reused foreign values", last.Config)
	}
}

func TestForeignRevisionVerifiedUpgrade(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	store, kc := delta2Native(t, f, false, false)
	allowForeignDeletion(kc)
	numbers := appendForeignRevisions(t, store)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	assertForeignReview(t, plan.Plan, numbers)
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	assertForeignApply(t, f, store, numbers[1]+1)
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("foreign revisions before the recorded target blocked rollback", err)
	}
}

func TestForeignRevisionKeepsVerifiedHistoryLocalRefusal(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	store, _ := delta2Native(t, f, false, false)
	appendForeignRevisions(t, store)
	f.release = localVersion(t, f.release.Version(), 1)
	before := deltaHistoryBytes(t, store)
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if p != nil {
		p.Close()
	}
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "local-release" || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("foreign revision hid verified history", err)
	}
}

func TestForeignManifestUsesNativeDeletionBaseline(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	store, kc := delta2Native(t, f, false, true)
	numbers := appendForeignRevisions(t, store)
	for _, number := range numbers {
		foreign, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		foreign.Manifest = ""
		if err = store.Update(foreign); err != nil {
			t.Fatal(err)
		}
	}
	kc.live = metav1.ObjectMeta{Labels: map[string]string{"app.kubernetes.io/managed-by": "Helm"}, Annotations: map[string]string{"meta.helm.sh/release-name": "oberth", "meta.helm.sh/release-namespace": "oberth"}}
	apply(t, f, prepare(t, f))
	if len(kc.deleted) != 0 {
		t.Fatal("native manifest omission was replaced by an installer baseline", kc.deleted)
	}
}

func TestForeignNewestRefusesRollbackButAllowsNewUpgrade(t *testing.T) {
	t.Parallel()
	for _, pending := range []bool{false, true} {
		t.Run(boolText(pending), func(t *testing.T) { foreignRollbackCase(t, pending) })
	}
}

func foreignRollbackCase(t *testing.T, pending bool) {
	t.Helper()
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	store, kc := delta2Native(t, f, false, false)
	appendForeignRevisions(t, store)
	allowForeignDeletion(kc)
	if pending {
		path := f.dir + "/rollback.json"
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		raw = bytes.Replace(raw, []byte(`"state":"completed"`), []byte(`"state":"pending"`), 1)
		if err = os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	before := deltaHistoryBytes(t, store)
	assertForeignRefusal(t, m.Rollback(t.Context()))
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("outside revisions blocked a new upgrade", err)
	}
	defer next.Plan.Close()
	if strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("overtaken record selected as repair")
	}
	if kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("foreign rollback changed state")
	}
}

func assertForeignRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "foreign-rollback" || len(refusal.Reason) > 80 || refusal.Remedy != "Run oberth upgrade for a new rollback point." || !strings.Contains(refusal.Detail, "Latest revision") {
		t.Fatal("missing actionable foreign rollback refusal", err)
	}
}

func TestForeignRevisionWithoutInstallerHistoryRefuses(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	store, kc := delta2Native(t, f, false, false)
	appendForeignRevisions(t, store)
	if _, err := store.Delete("oberth", 1); err != nil {
		t.Fatal(err)
	}
	before := deltaHistoryBytes(t, store)
	_, err := f.engine.Charts.Read(t.Context(), "oberth")
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "release-unowned" || strings.Contains(refusal.Remedy, "snapshot") {
		t.Fatal("unrelated history received damaged-release guidance", err)
	}
	if p, prepareErr := f.engine.Prepare(t.Context(), f.release, f.choices); prepareErr == nil {
		p.Close()
		t.Fatal("unrelated history accepted")
	}
	if kc.updates != 0 || !bytes.Equal(before, deltaHistoryBytes(t, store)) {
		t.Fatal("unrelated history changed")
	}
}

func TestForeignRevisionCannotHideDamagedRelease(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"name", "namespace", "chart", "metadata", "info"} {
		t.Run(shape, func(t *testing.T) {
			port, store, _ := pendingFixture(t, true)
			last, err := store.Last("oberth")
			if err != nil {
				t.Fatal(err)
			}
			damageRelease(last, shape)
			_, err = port.Read(t.Context(), "oberth")
			var refusal *installer.Problem
			if !errors.As(err, &refusal) {
				t.Fatal("damaged or unrelated release accepted", err)
			}
			if shape == "name" || shape == "namespace" {
				if refusal.Code != "target" {
					t.Fatal("wrong unrelated-release refusal", err)
				}
			} else if refusal.Code != "release-provenance" || !strings.Contains(refusal.Remedy, "copy the data volume at storage level") || !strings.Contains(refusal.Remedy, "repairing metadata by hand") {
				t.Fatal("damage lost its recovery remedy", err)
			}
		})
	}
}

func damageRelease(record *release.Release, shape string) {
	switch shape {
	case "name":
		record.Name = "another"
	case "namespace":
		record.Namespace = "another"
	case "chart":
		record.Chart = nil
	case "metadata":
		record.Chart.Metadata = nil
	case "info":
		record.Info = nil
	}
}

func TestForeignRollbackWithoutRecordOffersNewUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	store, _ := delta2Native(t, f, false, false)
	appendForeignRevisions(t, store)
	m := installer.Maintenance{Engine: f.engine}
	var refusal *installer.Problem
	if err := m.Rollback(t.Context()); !errors.As(err, &refusal) || refusal.Code != "rollback-missing" || !strings.Contains(refusal.Remedy, "oberth upgrade") {
		t.Fatal("missing rollback record has no usable next step", err)
	}
}

func TestOrdinaryUninstalledHistoryHasNoDeployedRevision(t *testing.T) {
	t.Parallel()
	port, store, _ := pendingFixture(t, true)
	for _, number := range []int{2, 3} {
		r, err := store.Get("oberth", number)
		if err != nil {
			t.Fatal(err)
		}
		r.Info.Status = release.StatusSuperseded
		if number == 3 {
			r.Info.Status = release.StatusUninstalled
		}
		if err = store.Update(r); err != nil {
			t.Fatal(err)
		}
	}
	h, err := port.Read(t.Context(), "oberth")
	if err != nil || h.Deployed.Number != 0 || h.Latest.Status != "uninstalled" {
		t.Fatal("ordinary removal acquired a deployed revision", h, err)
	}
}

func TestForeignVersionRequiresReviewedUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	apply(t, f, prepare(t, f))
	store, kc := delta2Native(t, f, false, false)
	appendForeignRevisions(t, store)
	last, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	last.Chart.Metadata.AppVersion = "v0.2.0"
	if err = store.Update(last); err != nil {
		t.Fatal(err)
	}
	assertInstallRefusal(t, f, "upgrade-required")
	if kc.updates != 0 {
		t.Fatal("version change reached native update")
	}
}

func assertInstallRefusal(t *testing.T, f fixture, code string) {
	t.Helper()
	p, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	if p != nil {
		p.Close()
	}
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}
