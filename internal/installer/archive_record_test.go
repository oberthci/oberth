package installer_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeRecordDoesNotPersistBackupDisplayHint(t *testing.T) {
	t.Parallel()
	f, maintenance := maintenanceFixture(t)
	plan, err := maintenance.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = maintenance.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.dir, "rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Archive map[string]any `json:"archive"`
	}
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if record.Archive["sha256"] == nil || record.Archive["report"] == nil {
		t.Fatal("archive recovery proof was not persisted")
	}
	if _, exists := record.Archive["store_snapshot_hint"]; exists {
		t.Fatal("display-only hint persisted in rollback record")
	}
}
