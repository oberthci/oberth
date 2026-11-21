package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
)

func TestCrossSchemaTimeoutCompletesThenUpgrades(t *testing.T) {
	t.Parallel()
	f, m, target := interruptedCrossSchemaFixture(t)
	next := loadFixture(t, releaseFor(t, "v0.2.0", 2))
	assertCrossSchemaRefusalsUnchanged(t, f, m, []installer.Release{target, next})
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	f.engine.CheckEndpoint = func(_ context.Context, profile installer.Profile) error {
		if profile.ServerVersion != f.charts.History.Latest.Version {
			return errors.New("wrong live version")
		}
		return nil
	}
	m.LiveState = func(context.Context) (stateport.Options, error) {
		return stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: 2}, nil
	}
	complete, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("ready target has no completion route", err)
	}
	defer complete.Plan.Close()
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) {
		t.Fatal("review wrote record")
	}
	result, err := m.Upgrade(t.Context(), complete, complete.Plan.Digest(), nil)
	if err != nil || result.Upgrade.To != target.Version() || len(f.charts.Applies) != applies {
		t.Fatal("completion reapplied chart", result.Upgrade, err)
	}
	record := readUpgradeRecord(t, f)
	if record.State != "completed" || record.BackupPath == "" || record.Options.Schema != 1 {
		t.Fatal("completion lost original recovery evidence", record.State)
	}
	advanceSchemaTwoUpgrade(t, f, m, next)

}

func assertCrossSchemaRefusalsUnchanged(t *testing.T, f fixture, m installer.Maintenance, targets []installer.Release) {
	t.Helper()
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	f.client.ClearActions()
	if err := m.Rollback(t.Context()); err == nil {
		t.Fatal("cross-schema rollback accepted")
	}
	for _, target := range targets {
		_, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
		assertUpgradeRepairRefusal(t, err)
	}
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || len(f.charts.Applies) != applies {
		t.Fatal("refusals changed state")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("refusal wrote cluster", action)
		}
	}
}

func advanceSchemaTwoUpgrade(t *testing.T, f fixture, m installer.Maintenance, next installer.Release) {
	t.Helper()
	m.Source = schemaTwoBackup(t)
	plan, err := m.PlanUpgrade(t.Context(), next, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("next weekly upgrade refused", err)
	}
	defer plan.Plan.Close()
	if _, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Version != next.Version() || readUpgradeRecord(t, f).State != "completed" {
		t.Fatal("next weekly upgrade incomplete")
	}
}

func schemaTwoBackup(t *testing.T) installer.SnapshotPort {
	t.Helper()
	source, err := statefake.New(stateport.DefaultProtocol(), stateport.BackupReport{Schema: 2, AuditHead: stateport.AuditHead{SHA256: strings.Repeat("a", 64)}, Integrity: true, ForeignKeys: true}, []byte("schema two database"))
	if err != nil {
		t.Fatal(err)
	}
	return source
}

func readUpgradeRecord(t *testing.T, f fixture) installer.Record {
	t.Helper()
	raw, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}

func TestCrossSchemaReadyTargetCanAdvanceDirectly(t *testing.T) {
	t.Parallel()
	f, m, target := interruptedCrossSchemaFixture(t)
	f.engine.CheckEndpoint = func(_ context.Context, profile installer.Profile) error {
		if profile.ServerVersion != f.charts.History.Latest.Version {
			return errors.New("wrong live target")
		}
		return nil
	}
	next := loadFixture(t, releaseFor(t, "v0.2.0", 2))
	m.Source = schemaTwoBackup(t)
	plan, err := m.PlanUpgrade(t.Context(), next, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	result, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	if err != nil || result.Upgrade.From != target.Version() {
		t.Fatal("ready target was not used as source", err)
	}
}

func TestCrossSchemaCompletionRechecksReadinessBeforeWrites(t *testing.T) {
	t.Parallel()
	f, m, target := interruptedCrossSchemaFixture(t)
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	before := directoryBytes(t, f.dir)
	applies := len(f.charts.Applies)
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("server stopped after review") }
	f.client.ClearActions()
	_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	assertUpgradeRepairRefusal(t, err)
	if !reflect.DeepEqual(before, directoryBytes(t, f.dir)) || len(f.charts.Applies) != applies {
		t.Fatal("refusal wrote state")
	}
	for _, action := range f.client.Actions() {
		if !readOnly(action) {
			t.Fatal("refusal wrote cluster", action)
		}
	}
}
