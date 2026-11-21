package installer_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeBackupsNameVersionTimeKeepThreeAndReportRemoval(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	untouched := filepath.Join(f.dir, "backup-personal.age")
	if err := os.WriteFile(untouched, []byte("private copy"), 0600); err != nil {
		t.Fatal(err)
	}
	var paths []string
	for i := range 4 {
		now := time.Date(2026, 11, 20, 12, i, 0, 0, time.UTC)
		f.engine.Now = func() time.Time { return now }
		target := loadFixture(t, releaseFor(t, fmt.Sprintf("v0.%d.0", i+1), 1))
		plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		result, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
		plan.Plan.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(result.BackupPath, target.Version()) || !strings.Contains(result.BackupPath, now.Format("20060102T150405.000000000Z")) {
			t.Fatal("backup path hides version or time", result.BackupPath)
		}
		if (i == 3) != strings.Contains(result.BackupReminder, "Removed older pre-upgrade backups: 1; kept the three newest.") {
			t.Fatal("pruning was not reported", result.BackupReminder)
		}
		paths = append(paths, result.BackupPath)
	}
	for i, path := range paths {
		_, err := os.Stat(path)
		if (i == 0) != os.IsNotExist(err) {
			t.Fatal("wrong backup retained", path, err)
		}
	}
	if _, err := os.Stat(untouched); err != nil {
		t.Fatal("unrelated archive removed", err)
	}
}
