package installer_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestFailedFirstVerifiedInstallRefusesLocalChart(t *testing.T) {
	t.Parallel()
	for _, profile := range []bool{true, false} {
		t.Run(boolText(profile), func(t *testing.T) { failedVerifiedLocalCase(t, profile) })
	}
}
func failedVerifiedLocalCase(t *testing.T, keepProfile bool) {
	t.Helper()
	f := failedVerifiedFirst(t)
	if !keepProfile {
		if err := os.Remove(f.dir + "/installation.json"); err != nil {
			t.Fatal(err)
		}
	}
	before, _ := os.ReadFile(f.dir + "/installation.json")
	previews, actions := len(f.charts.Previews), len(f.client.Actions())
	local := localVersion(t, f.release.Version(), 2)
	candidate, err := f.engine.Prepare(t.Context(), local, f.choices)
	if candidate != nil {
		candidate.Close()
	}
	var refusal *installer.Problem
	expected := "local-release"
	if !keepProfile {
		expected = "installation-owner"
	}
	if !errors.As(err, &refusal) || refusal.Code != expected {
		t.Fatal("failed first verified install accepted a local chart", err)
	}
	after, readErr := os.ReadFile(f.dir + "/installation.json")
	if !keepProfile && !os.IsNotExist(readErr) {
		t.Fatal("refusal recreated the profile", readErr)
	}
	if !bytes.Equal(before, after) || len(f.charts.Previews) != previews || len(f.charts.Applies) != 0 {
		t.Fatal("local refusal reached preview, writes or apply")
	}
	deltaAssertOnlyClusterReads(t, f, actions)
}

func failedVerifiedFirst(t *testing.T) fixture {
	t.Helper()
	f := setup(t)
	f.release = loadFixture(t, releaseFixture(t))
	f.engine.Charts = interruptedChart{ChartPort: f.charts, history: &f.charts.History}
	plan := prepare(t, f)
	if _, err := f.engine.Apply(t.Context(), plan, plan.Digest(), nil); !errors.Is(err, context.Canceled) {
		t.Fatal("fixture did not interrupt", err)
	}
	f.engine.Charts = f.charts
	f.charts.History.Latest.Status = "failed"
	f.charts.History.Verified = true
	if f.charts.History.Deployed.Number != 0 || f.charts.History.Latest.Kind != "verified" {
		t.Fatal("fixture has a predecessor or lacks verified provenance")
	}
	return f
}
