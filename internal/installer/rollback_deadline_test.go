package installer_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/installer/fake"
)

type observingRevert struct {
	*fake.Chart
	observe func(context.Context)
}

func (c observingRevert) Apply(ctx context.Context, r installer.ChartRequest) error {
	c.observe(ctx)
	return c.Chart.Apply(ctx, r)
}
func TestRollbackHasSeparateChartAndReadinessDeadlines(t *testing.T) {
	t.Parallel()
	f, m := maintenanceFixture(t)
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Plan.Close()
	if _, err = m.Upgrade(t.Context(), p, p.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
	var chart context.Context
	observed := false
	f.engine.Charts = observingRevert{f.charts, func(ctx context.Context) { chart = ctx }}
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		if !observed {
			observed = true
			return installer.NewProblem("starting", nil, "")
		}
		return nil
	}
	f.engine.Pause = func(ctx context.Context) error {
		assertRollbackDeadlines(t, chart, ctx)
		return nil
	}
	if err = m.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !observed {
		t.Fatal("rollback skipped readiness")
	}
}
func assertRollbackDeadlines(t *testing.T, chart, waiting context.Context) {
	t.Helper()
	if chart == nil || !errors.Is(chart.Err(), context.Canceled) {
		t.Fatal("rollback chart context remains live during readiness")
	}
	cd, cok := chart.Deadline()
	wd, wok := waiting.Deadline()
	if !cok || !wok || wd.Sub(cd) < 4*time.Minute || wd.Sub(cd) > 6*time.Minute {
		t.Fatal("rollback did not use separate 5m chart and 10m readiness limits")
	}
}
