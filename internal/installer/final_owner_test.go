package installer_test

import (
	"bytes"
	"github.com/oberthci/oberth/internal/installer"
	"os"
	"path/filepath"
	"testing"
)

func TestOwnerAppearsAtFinalWriteBoundary(t *testing.T) {
	f := setup(t)
	dir := filepath.Join(t.TempDir(), "absent", "profile")
	credentials, err := installer.OpenConfigDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer credentials.Close()
	f.engine.Credentials = credentials
	plan := prepare(t, f)
	winner := setup(t)
	winner.engine.Cluster, winner.engine.Charts = f.engine.Cluster, f.engine.Charts
	winnerPlan := prepare(t, winner)
	var before []byte
	var applies int
	_, err = f.engine.Apply(t.Context(), plan, plan.Digest(), func(phase installer.Phase) error {
		if phase == installer.ReleaseChecked {
			apply(t, winner, winnerPlan)
			before, applies = installationSnapshot(t, f), len(f.charts.Applies)
			f.client.ClearActions()
		}
		return nil
	})
	requireOwnerRefusal(t, err)
	if len(before) == 0 {
		t.Fatal("final boundary not reached")
	}
	deltaAssertOnlyClusterReads(t, f, 0)
	if !bytes.Equal(before, installationSnapshot(t, f)) || applies != len(f.charts.Applies) {
		t.Fatal("losing install changed the winner")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("losing install created local state", err)
	}
}
