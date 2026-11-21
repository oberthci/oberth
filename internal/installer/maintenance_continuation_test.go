package installer_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestExplicitEarlierVerifiedReleaseUsesUpgrade(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.charts.History.Deployed.Version = "v0.2.0"
	f.charts.History.Latest = f.charts.History.Deployed
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("explicit earlier verified upgrade refused", err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	if f.charts.History.Latest.Version != f.release.Version() {
		t.Fatal("earlier verified chart not applied")
	}
}
func TestSameVersionUpgradeCanRepair(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	f.charts.History.Deployed.Version = f.release.Version()
	f.charts.History.Latest = f.charts.History.Deployed
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal("same-version repair refused", err)
	}
	p.Plan.Close()
}
func TestMaintenanceWithoutEngineRefuses(t *testing.T) {
	t.Parallel()
	empty := installer.Maintenance{}
	if err := empty.Rollback(t.Context()); err == nil {
		t.Fatal("unwired rollback accepted")
	}
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = empty.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err == nil {
		t.Fatal("unwired upgrade accepted")
	}
}
