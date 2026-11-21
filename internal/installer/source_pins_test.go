package installer_test

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func appliedSourcePins(t *testing.T, f fixture) any {
	t.Helper()
	if len(f.charts.Applies) == 0 {
		t.Fatal("nothing was applied")
	}
	server, _ := f.charts.Applies[len(f.charts.Applies)-1].Values["server"].(map[string]any)
	runtime, _ := server["runtime"].(map[string]any)
	return runtime["source_policy"]
}

type pinUpgrade struct {
	m       installer.Maintenance
	release installer.Release
}

func (u pinUpgrade) run(t *testing.T, c installer.Choices, review string) {
	t.Helper()
	m := u.m
	plan, err := m.PlanUpgrade(t.Context(), u.release, c, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if !slices.Contains(plan.Plan.Review(), review) {
		t.Fatalf("upgrade review lacks %q:\n%s", review, strings.Join(plan.Plan.Review(), "\n"))
	}
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), func(installer.Phase) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

// The pairs are reviewed before anything is applied, applied as the server's
// policy, kept by a later upgrade that does not name them, and cleared only
// when the administrator says so.
func TestSourcePinsRoundTripThroughInstallAndUpgrade(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.choices.SourcePins = workflowpolicy.SourcePolicy{"origin/team/widget": {"origin/team/source", "origin/team/widget"}}
	p := prepare(t, f)
	if !slices.Contains(p.Review(), "Source pins: 2 pairs (--source-pin C=P)") || len(f.charts.Applies) != 0 {
		t.Fatalf("dry run does not review the pairs:\n%s", strings.Join(p.Review(), "\n"))
	}
	if detail := strings.Join(p.Detail(), "\n"); !strings.Contains(detail, `"source_policy": {`) || !strings.Contains(detail, `"origin/team/source"`) {
		t.Fatal("detail does not list the pairs", detail)
	}
	apply(t, f, p)
	want := map[string]any{"origin/team/widget": []any{"origin/team/source", "origin/team/widget"}}
	if got := appliedSourcePins(t, f); !reflect.DeepEqual(got, want) {
		t.Fatal("install applied another policy", got)
	}
	m := upgradeFixtureAfter(t, &f)
	u := pinUpgrade{m, f.release}
	u.run(t, installer.Choices{Context: "test"}, "Source pins: 2 pairs (--source-pin C=P)")
	if got := appliedSourcePins(t, f); !reflect.DeepEqual(got, want) {
		t.Fatal("upgrade without the option dropped the saved pairs", got)
	}
	added := installer.Choices{Context: "test", SourcePins: workflowpolicy.SourcePolicy{"origin/team/cli": {"origin/team/cli"}}}
	u.run(t, added, "Source pins: 2 -> 3 pairs (--source-pin C=P)")
	want["origin/team/cli"] = []any{"origin/team/cli"}
	if got := appliedSourcePins(t, f); !reflect.DeepEqual(got, want) {
		t.Fatal("an added pair replaced the saved ones", got)
	}
	u.run(t, installer.Choices{Context: "test", ClearSourcePins: true}, "Source pins: 3 -> 0 pairs (--source-pin C=P)")
	if got, _ := appliedSourcePins(t, f).(map[string]any); len(got) != 0 {
		t.Fatal("cleared pairs were applied", got)
	}
	plan, err := m.PlanUpgrade(t.Context(), f.release, installer.Choices{Context: "test"}, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if text := strings.Join(plan.Plan.Review(), "\n"); strings.Contains(text, "Source pins:") {
		t.Fatal("cleared pairs came back", text)
	}
}

// A rollback applies the source revision's pairs: pairs the upgrade added go,
// pairs it cleared come back.
func TestRollbackRestoresTheSourcePins(t *testing.T) {
	t.Parallel()
	saved := workflowpolicy.SourcePolicy{"origin/team/widget": {"origin/team/source"}}
	for name, tc := range map[string]struct {
		install, upgrade installer.Choices
		want             any
	}{
		"added":   {installer.Choices{Context: "test"}, installer.Choices{Context: "test", SourcePins: saved}, map[string]any{}},
		"cleared": {installer.Choices{Context: "test", SourcePins: saved}, installer.Choices{Context: "test", ClearSourcePins: true}, map[string]any{"origin/team/widget": []any{"origin/team/source"}}},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.choices = tc.install
			apply(t, f, prepare(t, f))
			m := upgradeFixtureAfter(t, &f)
			plan, err := m.PlanUpgrade(t.Context(), f.release, tc.upgrade, installer.UpgradeOptions{NoBackup: true})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Plan.Close()
			if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), func(installer.Phase) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err = m.Rollback(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := appliedSourcePins(t, f); !reflect.DeepEqual(got, tc.want) {
				t.Fatal("rollback applied the rolled-back pairs", got)
			}
		})
	}
}

// Completing an interrupted upgrade applies no chart, so it refuses a pin
// change instead of reviewing one it would drop; plain completion still works.
func TestCompletionRefusesSourcePinChanges(t *testing.T) {
	t.Parallel()
	f, m, target := pendingWithOlderBackups(t)
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	for _, c := range []installer.Choices{
		{Context: f.choices.Context, SourcePins: workflowpolicy.SourcePolicy{"origin/team/widget": {"origin/team/source"}}},
		{Context: f.choices.Context, ClearSourcePins: true},
	} {
		_, err := m.PlanUpgrade(t.Context(), target, c, installer.UpgradeOptions{})
		var problem *installer.Problem
		if !errors.As(err, &problem) || problem.Code != "source-pins-completion" {
			t.Fatal("completion accepted a pin change", err)
		}
	}
	complete, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("plain completion refused", err)
	}
	complete.Plan.Close()
}

func upgradeFixtureAfter(t *testing.T, f *fixture) installer.Maintenance {
	t.Helper()
	f.charts.History.Deployed.Version = "v0.0.9"
	f.charts.History.Latest = f.charts.History.Deployed
	setInstalledMetadata(t, *f, "local", 1)
	f.release = loadFixture(t, releaseFixture(t))
	source, options := backupFixture(t)
	return installer.Maintenance{Engine: f.engine, Source: source, Options: options, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		return releaseFor(t, version, 1).files, ctx.Err()
	}, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }}
}

// An invalid pair is refused while preparing, so neither a dry run nor an
// apply reaches the chart with it.
func TestInvalidSourcePinsRefusedBeforeApply(t *testing.T) {
	t.Parallel()
	many := make([]string, workflowpolicy.MaxSourceProducers+1)
	for i := range many {
		many[i] = fmt.Sprintf("origin/team/lib%d", i)
	}
	for name, pins := range map[string]workflowpolicy.SourcePolicy{
		"consumer":  {"widget": {"origin/team/source"}},
		"producer":  {"origin/team/widget": {"-x/team/source"}},
		"producers": {"origin/team/widget": many},
	} {
		t.Run(name, func(t *testing.T) {
			f := setup(t)
			f.choices.SourcePins = pins
			_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
			var problem *installer.Problem
			if !errors.As(err, &problem) || problem.Code != "source-pins" || problem.Detail == "" {
				t.Fatal("invalid pairs were not refused", err)
			}
			if len(f.charts.Previews) != 0 || len(f.charts.Applies) != 0 {
				t.Fatal("invalid pairs reached the chart")
			}
		})
	}
}
