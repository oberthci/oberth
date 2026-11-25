package installer_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// The printed schedule runs the installed release's own server image, pinned
// by digest; without a deployed digest it keeps the placeholder to edit.
func TestInstalledBackupScheduleRunsTheDeployedImageByDigest(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	digest := "sha256:" + strings.Repeat("5c", 32)
	f.charts.History.Deployed.Values["imageRepository"] = "ghcr.io/oberthci/oberth"
	f.charts.History.Deployed.Values["imageDigest"] = digest
	schedule, err := f.engine.InstalledBackupSchedule(t.Context(), "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(schedule, "image: ghcr.io/oberthci/oberth@"+digest+"\n") || strings.Contains(schedule, "REPLACE_WITH_PINNED_OBERTH_IMAGE") {
		t.Fatal("schedule does not run the installed image by digest")
	}
	if strings.Replace(schedule, "ghcr.io/oberthci/oberth@"+digest, "REPLACE_WITH_PINNED_OBERTH_IMAGE", 1) != installer.BackupSchedule {
		t.Fatal("schedule changed beyond its image")
	}
	f.charts.History.Deployed.Values["imageDigest"] = "unset"
	if schedule, err = f.engine.InstalledBackupSchedule(t.Context(), "oberth"); err != nil || schedule != installer.BackupSchedule {
		t.Fatal("an unpinned release invented an image", err)
	}
}
