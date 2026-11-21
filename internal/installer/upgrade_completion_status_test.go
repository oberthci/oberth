package installer_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestReadyTargetReappliesUndeployedRevision(t *testing.T) {
	t.Parallel()
	for _, status := range []string{"failed", "pending-upgrade"} {
		t.Run(status, func(t *testing.T) { reapplyReadyTarget(t, status) })
	}
}

func reapplyReadyTarget(t *testing.T, status string) {
	t.Helper()
	f, m, target := interruptedCrossSchemaFixture(t)
	f.charts.History.Deployed = readUpgradeRecord(t, f).Source
	f.charts.History.Latest.Status = status
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	m.LiveState = func(context.Context) (stateport.Options, error) {
		return stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: 2}, nil
	}
	m.Source = schemaTwoBackup(t)
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("ready target cannot finish its chart", err)
	}
	defer plan.Plan.Close()
	if strings.Contains(strings.Join(plan.Plan.Review(), "\n"), "complete the ready upgrade") {
		t.Fatal("undeployed revision offered completion without apply")
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || len(f.charts.Applies) != applies {
		t.Fatal("review changed installation state")
	}
	result, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	if err != nil || len(f.charts.Applies) != applies+1 || f.charts.History.Latest.Status != "deployed" {
		t.Fatal("remaining chart objects were not applied", err, len(f.charts.Applies))
	}
	record := readUpgradeRecord(t, f)
	if record.State != "completed" || record.Options.Schema != 2 || result.Upgrade.To != target.Version() || strings.Contains(result.Next, "Upgrade completed") {
		t.Fatal("ordinary reviewed apply lost its live schema or reported no-op completion", record, result.Next)
	}
}
