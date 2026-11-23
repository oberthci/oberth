package installer_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"k8s.io/utils/clock"
)

func TestSlowStartupFinishesOneUpgrade(t *testing.T) {
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
		useStartingEndpoint(t, f, 3*time.Minute)
		applies := len(f.charts.Applies)
		result, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
		if err != nil || result.Upgrade.To != f.release.Version() {
			t.Fatal("slow server required another upgrade", result.Upgrade, err)
		}
		if timeline.Since(started) != 3*time.Minute || len(f.charts.Applies) != applies+1 || readUpgradeRecord(t, f).State != "completed" {
			t.Fatal("upgrade did not finish once at readiness", timeline.Since(started))
		}
	})
}

func TestUnreadyStartupStopsAtReadinessBound(t *testing.T) {
	t.Parallel()
	for _, readyAfter := range []time.Duration{time.Hour, 0} {
		t.Run(readyAfter.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f, m := maintenanceFixture(t)
				plan, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{})
				if err != nil {
					t.Fatal(err)
				}
				defer plan.Plan.Close()
				timeline := clock.RealClock{}
				started := timeline.Now()
				useStartingEndpoint(t, f, readyAfter)
				_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
				if !errors.Is(err, context.DeadlineExceeded) || timeline.Since(started) != installer.ReadinessTimeout {
					t.Fatal("stuck server did not stop at the bound", timeline.Since(started), err)
				}
				if !strings.Contains(err.Error(), "The server did not become ready before the deadline.") {
					t.Fatal("readiness deadline was not explained", err)
				}
				if readUpgradeRecord(t, f).State != "pending" {
					t.Fatal("failed upgrade lost its recovery record")
				}
			})
		})
	}
}

// The fake stalls dials for two minutes, then serves 503 until readyAfter.
// Zero keeps every dial stalled; no production cause is inferred from this fake.
func useStartingEndpoint(t *testing.T, f fixture, readyAfter time.Duration) {
	t.Helper()
	config := serverTLS(t, f)
	timeline := clock.RealClock{}
	started := timeline.Now()
	var workers sync.WaitGroup
	t.Cleanup(workers.Wait)
	verifier := installer.EndpointVerifier{Now: config.Time, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		if timeline.Since(started) < 2*time.Minute || readyAfter == 0 {
			return stalledStartupDial(ctx)
		}
		client, remote := net.Pipe()
		status := http.StatusServiceUnavailable
		if timeline.Since(started) >= readyAfter {
			status = http.StatusOK
		}
		workers.Go(func() {
			serveTLSCase(ctx, remote, config, tlsCase{status: status, body: `{"ready":true,"version":"` + f.release.Version() + `"}`})
		})
		return client, nil
	}}
	f.engine.CheckEndpoint = func(ctx context.Context, profile installer.Profile) error {
		profile.ServerURL = "https://server.oberth.ci"
		return verifier.Verify(ctx, profile)
	}
}

func stalledStartupDial(ctx context.Context) (net.Conn, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestStartupDialWaitsForCancellation(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		timeline := clock.RealClock{}
		started := timeline.Now()
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
		defer cancel()
		conn, err := stalledStartupDial(ctx)
		if conn != nil || !errors.Is(err, ctx.Err()) || timeline.Since(started) != 3*time.Minute {
			t.Fatal("fake dial ended before caller cancellation", err, timeline.Since(started))
		}
	})
}
