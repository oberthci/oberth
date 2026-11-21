package installer_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
)

func TestReadinessLimitUsesPlainSeconds(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		limit time.Duration
		text  string
	}{
		{90 * time.Second, "waiting for server (up to 90 s)"},
		{150 * time.Second, "waiting for server (up to 150 s)"},
		{time.Millisecond, "waiting for server (up to 1 s)"},
	} {
		t.Run(tc.text, func(t *testing.T) {
			f := setup(t)
			f.engine.Timeout = tc.limit
			p := prepare(t, f)
			seen := false
			_, err := f.engine.Apply(t.Context(), p, p.Digest(), func(phase installer.Phase) error {
				if strings.HasPrefix(string(phase), "waiting for server") {
					seen = true
					if string(phase) != tc.text || len(phase) > 40 {
						t.Fatal("unclear duration", phase)
					}
					return context.Canceled
				}
				return nil
			})
			if !seen || !errors.Is(err, context.Canceled) {
				t.Fatal("readiness limit was not announced", seen, err)
			}
		})
	}
}

func TestChartStateUsesAvailableRecoveryCommand(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	f.charts.History = installer.History{}
	f.charts.History.Latest = installer.Revision{Number: 1, Status: "unknown", Kind: "local"}
	_, err := f.engine.Prepare(t.Context(), f.release, f.choices)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "chart-state" {
		t.Fatal(err)
	}
	if !strings.Contains(refusal.Remedy, "helm history oberth -n <namespace>") || !strings.Contains(refusal.Remedy, "preserve the data volume") {
		t.Fatal("recovery remedy requires unavailable commands", refusal.Remedy)
	}
}

func TestDefaultReadinessWaitEndsAsSoonAsReady(t *testing.T) {
	t.Parallel()
	f := setup(t)
	checks, pauses := 0, 0
	f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		checks++
		if checks < 3 {
			return installer.NewProblem("starting", nil, "")
		}
		return nil
	}
	f.engine.Pause = func(context.Context) error { pauses++; return nil }
	apply(t, f, prepare(t, f))
	if checks != 3 || pauses != 2 {
		t.Fatal("ready endpoint did not stop the wait", checks, pauses)
	}
	if installer.ReadinessTimeout != 10*time.Minute {
		t.Fatal("readiness default is not ten minutes")
	}
}
