package installer_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
)

// A ready endpoint whose live metadata cannot be read is repaired as before,
// never sent round the same review again.
func TestReadyTargetWithUnreadableMetadataIsRepaired(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	target := loadFixture(t, releaseFor(t, "v0.1.0", 1))
	plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return installer.NewProblem("timeout", nil, "") }
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil || readUpgradeRecord(t, f).State != "pending" {
		t.Fatal("fixture did not leave a pending record", err)
	}
	plan.Plan.Close()
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	reads := 0
	m.LiveState = func(context.Context) (stateport.Options, error) {
		reads++
		return stateport.Options{}, errors.New("administrator metadata refused")
	}
	for attempt := 0; attempt < 2; attempt++ {
		review, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal("unverifiable completion refused instead of repaired", err)
		}
		if text := strings.Join(review.Plan.Review(), "\n"); !strings.Contains(text, "Repair: roll back the interrupted change, then apply this release.") {
			t.Fatal("unverifiable completion not reviewed for repair", text)
		}
		review.Plan.Close()
	}
	repair, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer repair.Plan.Close()
	applies := len(f.charts.Applies)
	if _, err = m.Upgrade(t.Context(), repair, repair.Plan.Digest(), nil); err != nil {
		t.Fatal("repair failed", err)
	}
	if reads == 0 || len(f.charts.Applies) != applies+2 || readUpgradeRecord(t, f).State != "completed" {
		t.Fatal("repair did not roll back and apply again", reads, len(f.charts.Applies)-applies)
	}
}
