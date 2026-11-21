package installer_test

import (
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeReviewNamesTheBackupDirectoryNotAFile(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	before := directoryBytes(t, f.dir)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	review := plan.Plan.Review()
	if !slices.Contains(review, "Pre-upgrade backup will be written to "+f.dir+"/") {
		t.Fatal("review does not say where the backup goes", review)
	}
	if text := strings.Join(review, "\n"); strings.Contains(text, ".age") {
		t.Fatal("review names a backup file it does not write", text)
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) {
		t.Fatal("review wrote a file")
	}
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if record := readUpgradeRecord(t, f); !strings.HasPrefix(record.BackupPath, f.dir+"/backup-") {
		t.Fatal("apply wrote no backup in the reviewed directory", record.BackupPath)
	} else if _, err = os.Stat(record.BackupPath); err != nil {
		t.Fatal(err)
	}
}
