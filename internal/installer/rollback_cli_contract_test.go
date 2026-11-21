package installer_test

import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestRollbackWithoutRestoreTransportRefusesSchemaChangeBeforeMutation(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.release = loadFixture(t, releaseFor(t, "v0.1.0", 2))
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	record, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	before := rollbackSnapshot{Record: record, History: f.charts.History, Applies: len(f.charts.Applies)}
	m.ReleaseFiles = nil
	err = m.Rollback(t.Context())
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Reason != "Rollback across a database schema change is unavailable." || !strings.Contains(problem.Remedy, "recovery is manual") {
		t.Fatal("schema refusal is not actionable", err)
	}
	assertRollbackUnchanged(t, f, before)
}

func TestRollbackWithoutRestoreTransportUsesSameSchema(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Version != "v0.0.9" {
		t.Fatal("earlier release not applied")
	}
}
