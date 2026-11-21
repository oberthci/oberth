package installer_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestUpgradeFailureKeepsCauseWithoutInstallRemedy(t *testing.T) {
	t.Parallel()
	for _, schema := range []int{1, 2} {
		t.Run(fmt.Sprint(schema), func(t *testing.T) {
			f, m := maintenanceFixture(t)
			target := loadFixture(t, releaseFor(t, "v0.1.0", schema))
			plan, err := m.PlanUpgrade(t.Context(), target, f.choices, installer.UpgradeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer plan.Plan.Close()
			f.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
				return installer.NewProblem("starting", nil, "")
			}
			f.engine.Pause = func(context.Context) error { return context.DeadlineExceeded }
			_, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
			var refusal *installer.Problem
			if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &refusal) {
				t.Fatal("upgrade lost its readiness failure", err)
			}
			cause := installer.NewProblem("timeout", nil, "")
			if !strings.HasSuffix(refusal.Detail, cause.Reason) || strings.Contains(refusal.Detail, cause.Remedy) || strings.Contains(refusal.Detail, "oberth install") {
				t.Fatal("upgrade detail includes an installation remedy or omits its cause", refusal.Detail)
			}
			if !strings.Contains(refusal.Detail, "Rollback record: "+f.dir+"/rollback.json") || readUpgradeRecord(t, f).State != "pending" {
				t.Fatal("failed upgrade lost its recovery record", refusal.Detail)
			}
		})
	}
}
