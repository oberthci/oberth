package installer_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/utils/clock"
)

func TestUpgradeWaitsForReviewedVersionAfterApply(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f, m := maintenanceFixture(t)
		plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Plan.Close()
		timeline := clock.RealClock{}
		started := timeline.Now()
		config := serverTLS(t, f)
		var workers sync.WaitGroup
		defer workers.Wait()
		verifier := installer.EndpointVerifier{Now: config.Time, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			version := "v0.0.9"
			if timeline.Since(started) >= time.Second {
				version = f.release.Version()
			}
			client, remote := net.Pipe()
			workers.Go(func() {
				serveTLSCase(ctx, remote, config, tlsCase{status: http.StatusOK, body: `{"ready":true,"version":"` + version + `"}`})
			})
			return client, nil
		}}
		f.engine.CheckEndpoint = func(ctx context.Context, profile installer.Profile) error {
			profile.ServerURL = "https://server.oberth.ci"
			return verifier.Verify(ctx, profile)
		}
		applies := len(f.charts.Applies)
		result, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
		if err != nil || result.Upgrade.To != f.release.Version() || timeline.Since(started) != time.Second {
			t.Fatal("old ready server ended the upgrade wait", result.Upgrade, err, timeline.Since(started))
		}
		if len(f.charts.Applies) != applies+1 || readUpgradeRecord(t, f).State != "completed" {
			t.Fatal("waiting for the new version changed the apply or completion count")
		}
	})
}

func TestWrongReadyVersionCannotCompleteUpgrade(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f, m := maintenanceFixture(t)
		plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer plan.Plan.Close()
		checks := 0
		f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
			checks++
			return installer.NewProblem("ready-version", nil, "Server version: v0.0.9")
		}
		timeline := clock.RealClock{}
		started := timeline.Now()
		_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
		if !errors.Is(err, context.DeadlineExceeded) || timeline.Since(started) != installer.ReadinessTimeout || checks < 2 {
			t.Fatal("wrong ready version bypassed the wait or its bound", checks, err, timeline.Since(started))
		}
		if readUpgradeRecord(t, f).State != "pending" {
			t.Fatal("wrong version completed the recovery record")
		}
	})
}
