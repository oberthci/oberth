package installer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

type chartPhase struct {
	installer.ChartPort
	observe func(context.Context) error
}

func (c chartPhase) Apply(ctx context.Context, r installer.ChartRequest) error {
	if err := c.observe(ctx); err != nil {
		return err
	}
	return c.ChartPort.Apply(ctx, r)
}
func TestChartAndServerHaveIndependentLimits(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	var chartContext context.Context
	var chartDeadline time.Time
	f.engine.Charts = chartPhase{ChartPort: f.charts, observe: func(ctx context.Context) error {
		chartContext = ctx
		var ok bool
		chartDeadline, ok = ctx.Deadline()
		if !ok {
			t.Fatal("unbounded chart phase")
		}
		return nil
	}}
	announced := false
	f.engine.CheckEndpoint = func(ctx context.Context, _ installer.Profile) error {
		if !announced {
			t.Fatal("readiness limit was not announced")
		}
		if !errors.Is(chartContext.Err(), context.Canceled) {
			t.Fatal("chart phase still owns readiness context")
		}
		deadline, ok := ctx.Deadline()
		if !ok || deadline.Sub(chartDeadline) < 4*time.Minute || deadline.Sub(chartDeadline) > 6*time.Minute {
			t.Fatal("expected independent 5m chart and 10m server limits")
		}
		return nil
	}
	if _, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
		if strings.HasPrefix(string(phase), string(installer.Waiting)) {
			announced = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
func TestChartDeadlineNamesChartPhase(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	f.engine.Charts = chartPhase{ChartPort: f.charts, observe: func(context.Context) error { return context.DeadlineExceeded }}
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "chart-timeout" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("chart deadline mislabeled", err)
	}
}
func TestServerDeadlineNamesServerPhase(t *testing.T) {
	t.Parallel()
	f := setup(t)
	p := prepare(t, f)
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return context.DeadlineExceeded }
	_, err := f.engine.Apply(t.Context(), p, p.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "timeout" || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("server deadline mislabeled", err)
	}
}
