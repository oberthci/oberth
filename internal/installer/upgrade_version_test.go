package installer_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestReadyOtherVersionRefusalDoesNotLoop(t *testing.T) {
	t.Parallel()
	f, m, target := interruptedCrossSchemaFixture(t)
	f.engine.CheckEndpoint = func(_ context.Context, profile installer.Profile) error {
		return installer.NewProblem("ready-version", nil, "Server version: v0.0.9\nReviewed release: "+profile.ServerVersion)
	}
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	f.client.ClearActions()
	_, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "ready-version" || !strings.Contains(refusal.Reason, "version") {
		t.Fatal("ready server mismatch got the wait-and-retry refusal", err)
	}
	if strings.Contains(refusal.Remedy, "oberth upgrade") || !strings.Contains(refusal.Remedy, "Helm history") {
		t.Fatal("version mismatch loops instead of naming an inspection", refusal)
	}
	for _, fact := range []string{"Server version: v0.0.9", "Reviewed release: " + target.Version(), "Rollback record:", "Backup key:"} {
		if !strings.Contains(refusal.Detail, fact) {
			t.Fatal("version refusal lost its evidence", fact, refusal.Detail)
		}
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || len(f.charts.Applies) != applies {
		t.Fatal("version refusal changed installation state")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("version refusal wrote cluster state", action)
		}
	}
}
