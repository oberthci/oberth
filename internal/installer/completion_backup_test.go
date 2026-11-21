package installer_test

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// Completing a record changes nothing, so it neither announces, writes nor
// prunes a pre-upgrade backup.
func TestCompletingARecordTouchesNoBackup(t *testing.T) {
	t.Parallel()
	f, m, target := pendingWithOlderBackups(t)
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	complete, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer complete.Plan.Close()
	text := strings.Join(complete.Plan.Review(), "\n")
	if !strings.Contains(text, "Recovery: complete the ready upgrade") || strings.Contains(text, "\nBackup:") || strings.Contains(text, "Pre-upgrade backup") {
		t.Fatal("completion review announces a backup", text)
	}
	before := backupNames(t, f.dir)
	result, err := m.Upgrade(t.Context(), complete, complete.Plan.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if result.BackupPath != "" || result.BackupReminder != "" || !reflect.DeepEqual(before, backupNames(t, f.dir)) {
		t.Fatal("completion wrote, named or pruned a backup", result.BackupPath, result.BackupReminder)
	}
}

// pendingWithOlderBackups leaves a same-schema upgrade whose readiness wait
// expired, with its own backup and three older ones in the profile.
func pendingWithOlderBackups(t *testing.T) (fixture, installer.Maintenance, installer.Release) {
	t.Helper()
	f, m := maintenanceFixture(t)
	target := loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return installer.NewProblem("timeout", nil, "") }
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("fixture did not leave a pending record")
	}
	for _, stamp := range []string{"20260101T000000.000000000Z", "20260102T000000.000000000Z", "20260103T000000.000000000Z"} {
		name := "backup-" + stamp + "-" + strings.Repeat("a", 24) + "-v0.0.9.age"
		if err = os.WriteFile(filepath.Join(f.dir, name), []byte("older"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return f, m, target
}

func backupNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "backup-") {
			names = append(names, entry.Name())
		}
	}
	return names
}
