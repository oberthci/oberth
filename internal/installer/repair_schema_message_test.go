package installer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestInterruptedCrossSchemaUpgradeNamesRepairAndManualRecovery(t *testing.T) {
	t.Parallel()
	f, maintenance, target := interruptedCrossSchemaFixture(t)
	path := filepath.Join(f.dir, "rollback.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	previews, applies := len(f.charts.Previews), len(f.charts.Applies)
	_, err = maintenance.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	assertUpgradeRepairRefusal(t, err)
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) || len(f.charts.Previews) != previews || len(f.charts.Applies) != applies {
		t.Fatal("refusal changed the installation", err)
	}
	var refusal *installer.Problem
	if err = maintenance.Rollback(t.Context()); !errors.As(err, &refusal) || refusal.Code != "rollback-schema" || refusal.Remedy != "Keep the backup, backup.agekey and rollback.json in this profile; recovery is manual in this version." {
		t.Fatal("rollback lost its own schema refusal", err)
	}
}

func interruptedCrossSchemaFixture(t *testing.T) (fixture, installer.Maintenance, installer.Release) {
	t.Helper()
	f, maintenance := maintenanceFixture(t)
	target := loadFixture(t, releaseFor(t, "v0.1.0", 2))
	plan, err := maintenance.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		return installer.NewProblem("starting", nil, "image is still pulling")
	}
	f.engine.Pause = func(context.Context) error { return context.DeadlineExceeded }
	_, err = maintenance.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &refusal) || refusal.Code != "upgrade-schema-failed" || !strings.Contains(refusal.Remedy, "oberth system status") || strings.Contains(refusal.Remedy, "rollback") {
		t.Fatal("wrong failed-upgrade remedy", err)
	}
	for _, detail := range []string{"Encrypted pre-upgrade backup: " + f.dir + "/backup-", "Backup key: " + f.dir + "/backup.agekey", "Recovery is manual in this version."} {
		if !strings.Contains(refusal.Detail, detail) {
			t.Fatal("recovery custody missing", refusal.Detail)
		}
	}

	return f, maintenance, target
}

func assertUpgradeRepairRefusal(t *testing.T, err error) {
	t.Helper()
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "repair-schema" || refusal.Reason != "Upgrade repair across a schema change is unavailable." {
		t.Fatal("upgrade refused with another operation's reason", err)
	}
	if refusal.Remedy != "When oberth system status is ready, retry oberth upgrade; otherwise keep this profile for manual recovery." || len([]rune(refusal.Reason)) > 80 || len([]rune(refusal.Remedy)) > 120 || !strings.Contains(refusal.Remedy, "oberth system status") {
		t.Fatal("upgrade repair does not name usable recovery", refusal.Remedy)
	}
}
