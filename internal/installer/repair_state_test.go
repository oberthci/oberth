package installer_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestRecordedRepairSelectsStateBeforeLiveMetadata(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("injected startup failure") }
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("startup failure not injected")
	}
	calls := 0
	m.Options.Schema = 99
	m.LiveState = func(context.Context) (stateport.Options, error) {
		calls++
		return stateport.Options{}, errors.New("server stopped")
	}
	before := len(f.charts.Applies)
	next, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal("valid repair was gated on server", err)
	}
	defer next.Plan.Close()
	if calls != 0 || len(f.charts.Applies) != before || !strings.Contains(strings.Join(next.Plan.Review(), "\n"), "Repair:") {
		t.Fatal("repair did not stay read-only or bypass live state", calls)
	}
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	if _, err = m.Upgrade(t.Context(), next, next.Plan.Digest(), nil); err != nil {
		t.Fatal("repair cannot apply", err)
	}
}

func TestFreshUpgradeRequiresLiveStateWithoutWritingRecord(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	sentinel := errors.New("live metadata unavailable")
	calls := 0
	m.LiveState = func(context.Context) (stateport.Options, error) { calls++; return stateport.Options{}, sentinel }
	before := len(f.charts.Previews)
	plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if plan != nil || !errors.Is(err, sentinel) || calls != 1 || len(f.charts.Previews) != before {
		t.Fatal("new upgrade ignored live metadata", calls, err)
	}
	if _, err = os.Stat(f.dir + "/rollback.json"); !os.IsNotExist(err) {
		t.Fatal("fresh failure wrote record", err)
	}
}

func TestRecordedRepairRefusesChangedTargetAndCluster(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"version", "cluster", "schema", "record uid", "record context"} {
		t.Run(kind, func(t *testing.T) { recordedRepairRefusal(t, kind) })
	}
}

func recordedRepairRefusal(t *testing.T, kind string) {
	t.Helper()
	f, m := maintenanceFixture(t)
	schema := 1
	if kind == "schema" {
		schema = 2
	}
	target := loadFixture(t, releaseFor(t, "v0.1.0", schema))
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("startup stopped") }
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("startup failure missing")
	}
	if kind == "version" {
		target = loadFixture(t, releaseFor(t, "v0.2.0", schema))
	}
	if kind == "cluster" {
		f.engine.Cluster.Context = "other"
	}
	m.LiveState = func(context.Context) (stateport.Options, error) {
		t.Fatal("unbound record reached live fallback")
		return stateport.Options{}, nil
	}
	before, err := os.ReadFile(f.dir + "/rollback.json")
	if err != nil {
		t.Fatal(err)
	}
	before = alterRepairRecord(t, kind, f.dir, before)
	snapshot := rollbackSnapshot{Record: before, History: f.charts.History, Applies: len(f.charts.Applies)}
	if p, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{NoBackup: true}); err == nil {
		p.Plan.Close()
		t.Fatal("unbound repair accepted")
	}
	assertRollbackUnchanged(t, f, snapshot)
}

func alterRepairRecord(t *testing.T, kind, dir string, raw []byte) []byte {
	t.Helper()
	if kind != "record uid" && kind != "record context" {
		return raw
	}
	var record installer.Record
	if err := json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	if kind == "record uid" {
		record.ClusterUID = "foreign"
	} else {
		record.Context = "foreign"
	}
	result, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(dir+"/rollback.json", result, 0600); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestNewUpgradeFloorUsesLiveSchemaEvenWhenOptionsClaimAnother(t *testing.T) {
	t.Parallel()
	for _, schema := range []int{1, 7} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			f, m := maintenanceFixture(t)
			m.Options.Schema = 99
			calls := 0
			m.LiveState = func(context.Context) (stateport.Options, error) {
				calls++
				return stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: schema}, nil
			}
			plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
			if calls != 1 {
				t.Fatal("fresh upgrade did not use live metadata", calls)
			}
			if schema == 1 {
				if err != nil {
					t.Fatal(err)
				}
				plan.Plan.Close()
				return
			}
			var refusal *installer.Problem
			if plan != nil || !errors.As(err, &refusal) || refusal.Code != "schema-downgrade" {
				t.Fatal("live schema floor bypassed", err)
			}
		})
	}
}
