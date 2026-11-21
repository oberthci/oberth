package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/oberthci/oberth/internal/stateport"
	"io"
	"io/fs"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
)

func maintenanceFixture(t *testing.T) (fixture, installer.Maintenance) {
	t.Helper()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History.Deployed.Version = "v0.0.9"
	f.charts.History.Latest = f.charts.History.Deployed
	setInstalledMetadata(t, f, "local", 1)
	f.release = loadFixture(t, releaseFixture(t))
	source, options := backupFixture(t)
	return f, installer.Maintenance{Engine: f.engine, Source: source, Options: options, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		return releaseFor(t, version, 1).files, ctx.Err()
	}, ActiveRuns: func(context.Context, string) (int, error) { return 3, nil }}
}
func TestRollbackLineage(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	text := strings.Join(plan.Plan.Review(), "\n")
	if !strings.Contains(text, "Runs: 3; unlabelled runs not adopted; finish before upgrade.") {
		t.Fatal("adoption warning missing", text)
	}
	_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.Checking {
			if _, e := os.Stat(filepath.Join(f.dir, "rollback.json")); e != nil {
				t.Fatal("record saved after first cluster write", e)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	assertRollbackRecord(t, f)
	good := f.charts.History
	f.charts.History.Latest.Number += 7
	if err = m.Rollback(t.Context()); err == nil {
		t.Fatal("unrelated revision accepted")
	}
	f.charts.History = good
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Version != "v0.0.9" {
		t.Fatal("historical chart not restored")
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal("completed rollback not idempotent", err)
	}
}

type canceledRevert struct{ *fake.Chart }

func (c canceledRevert) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := c.Chart.Apply(ctx, r); err != nil {
		return err
	}
	return context.Canceled
}
func TestInterruptedRollbackResumes(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	f.engine.Charts = canceledRevert{f.charts}
	if err = m.Rollback(t.Context()); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	f.engine.Charts = f.charts
	revision := f.charts.History.Latest.Number
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Number != revision {
		t.Fatal("replayed a completed native rollback")
	}
}
func TestUnsignedUpgradeRefusesBeforeWrites(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	unsigned := setup(t).release
	before, _ := os.ReadDir(f.dir)
	if p, err := m.PlanUpgrade(t.Context(), unsigned, f.choices, installer.UpgradeOptions{}); err == nil {
		p.Plan.Close()
		t.Fatal("unsigned upgrade accepted")
	}
	after, _ := os.ReadDir(f.dir)
	if len(before) != len(after) {
		t.Fatal("unverified release wrote local state")
	}
}

func TestFailedUpgradeIsRepairedByNextUpgrade(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("startup refused") }
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil || !strings.Contains(err.Error(), "oberth rollback") {
		t.Fatal("no one-line way back", err)
	}
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Plan.Close()
	if !strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("repair not reviewed")
	}
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	if _, err = m.Upgrade(t.Context(), next, next.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Version != f.release.Version() {
		t.Fatal("failed upgrade was not restored and reapplied")
	}
}

func assertRollbackRecord(t *testing.T, f fixture) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.dir, "rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err = json.Unmarshal(raw, &record); err != nil || record.Archive == nil {
		t.Fatal("backup not recorded", err)
	}
}

type snapshotCheck struct {
	installer.SnapshotPort
	check func()
}

func (s snapshotCheck) Snapshot(ctx context.Context, o stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	s.check()
	return s.SnapshotPort.Snapshot(ctx, o, out)
}
func TestRollbackRecordPrecedesBackup(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	m.Source = snapshotCheck{m.Source, func() {
		if _, err := os.Stat(filepath.Join(f.dir, "rollback.json")); err != nil {
			t.Fatal("backup began before rollback record", err)
		}
	}}
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestRollbackRefusesRecreatedCluster(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = "recreated"
	if _, err = f.client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(t.Context()); err == nil {
		t.Fatal("rollback crossed cluster identity")
	}
}
