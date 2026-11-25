package installer_test

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

// An upgrade that chose port 443 rolls back to an earlier chart that declares
// servicePort with 443 carried; the earlier revision never saved a port.
func TestRollbackCarriesChosenServicePortToDeclaringChart(t *testing.T) {
	f, m := upgradeWithServicePort(t, releaseFor(t, "v0.30.99", 6))
	if err := m.Rollback(t.Context()); err != nil {
		t.Fatal("rollback after --service-port refused", err)
	}
	last := f.charts.Applies[len(f.charts.Applies)-1]
	if last.Chart.Metadata.AppVersion != "v0.30.99" || last.Values["servicePort"] != 443 {
		t.Fatal("rollback did not carry the chosen Service port", last.Chart.Metadata.AppVersion, last.Values["servicePort"])
	}
}

// An earlier chart without servicePort would move the Service to 8443
// silently; rollback refuses before any chart change and names the port.
func TestRollbackRefusesChosenServicePortOnFixedPortChart(t *testing.T) {
	old := archivedRelease(t, "v0.30.15", "508a087f17d41cc00b710590a840404424859a0346e96b019ea526a38e69effd")
	f, m := upgradeWithServicePort(t, old)
	applies := len(f.charts.Applies)
	err := m.Rollback(t.Context())
	var problem *installer.Problem
	if !errors.As(err, &problem) || problem.Code != "rollback-service-port" {
		t.Fatal("rollback to a fixed-port chart was not refused clearly", err)
	}
	if len(problem.Reason) > 40 || len(problem.Remedy) > 120 || !strings.Contains(problem.Remedy, "--service-port 8443") || !strings.Contains(problem.Detail, "HTTPS Service port: 443") || !strings.Contains(problem.Detail, "v0.30.15") {
		t.Fatal("rollback refusal does not name the port and release", problem.Reason, problem.Remedy, problem.Detail)
	}
	if len(f.charts.Applies) != applies {
		t.Fatal("refused rollback changed the chart")
	}
}

func upgradeWithServicePort(t *testing.T, old releaseData) (fixture, installer.Maintenance) {
	t.Helper()
	f := setup(t)
	apply(t, f, prepare(t, f))
	setHistoricalRevision(t, f, old)
	m := installer.Maintenance{Engine: f.engine, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }, Options: stateport.Options{Schema: 6, Protocol: stateport.DefaultProtocol()}, ReleaseFiles: func(ctx context.Context, version string) (fs.FS, error) {
		if version != old.version {
			t.Fatal("wrong rollback release", version)
		}
		return old.files, ctx.Err()
	}}
	f.choices.HTTPSServicePort = 443
	p, err := m.PlanUpgrade(t.Context(), loadFixture(t, releaseFor(t, "v0.31.0", 6)), f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	return f, m
}
