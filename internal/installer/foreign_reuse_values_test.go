package installer_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chartutil"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage"
	"helm.sh/helm/v3/pkg/strvals"
)

func TestNativeReuseValuesRequiresReviewedRecovery(t *testing.T) {
	t.Parallel()
	if len(fmt.Sprintf(foreignReviewLine, math.MaxInt)) > 80 {
		t.Fatal("outside-change review cannot show a complete revision number")
	}
	for _, operation := range []string{"install", "upgrade"} {
		t.Run(operation, func(t *testing.T) { nativeReuseValuesCase(t, operation) })
	}
}

func nativeReuseValuesCase(t *testing.T, operation string) {
	t.Helper()
	f, store, kc := foreignInstallFixture(t, false)
	old := prepare(t, f)
	defer old.Close()
	outside := nativeReuseValuesUpgrade(t, store, kc)
	unchanged := unchangedInstallation(t, f, store, kc)
	assertReuseValuesHistory(t, f)
	p, run := reviewedReuseValuesRecovery(t, f, operation)
	defer p.Close()
	assertForeignReview(t, p, []int{2})
	unchanged()
	for _, approval := range []string{"", old.Digest()} {
		var refusal *installer.Problem
		if err := run(approval); !errors.As(err, &refusal) || refusal.Code != "review" {
			t.Fatal("outside revision was applied without its own approval", err)
		}
		unchanged()
	}
	if err := run(p.Digest()); err != nil {
		t.Fatal("approved recovery over the native reuse-values revision failed", err)
	}
	assertRecoveredReuseValues(t, store, kc, outside)
}

func assertReuseValuesHistory(t *testing.T, f fixture) {
	t.Helper()
	h, err := f.engine.Charts.Read(t.Context(), "oberth")
	if err != nil || h.Latest.Number != 2 || h.Latest.Kind != "foreign" || h.Latest.Installer() || h.Latest.Values != nil || h.Latest.RollbackID != "" || h.Choices.Number != 1 {
		t.Fatal("native read vouched for a stale reuse-values binding", err)
	}
}

func assertRecoveredReuseValues(t *testing.T, store *storage.Storage, kc *deltaDeletionKube, outside *release.Release) {
	t.Helper()
	last, err := store.Last("oberth")
	if err != nil || last.Version != 3 || last.Info.Status != release.StatusDeployed || last.Manifest == outside.Manifest || kc.updates != 2 {
		t.Fatal("approval did not render over the outside revision", err)
	}
	if last.Labels["oberth.ci/kind-revision"] != installer.RevisionBindingForTest(3, "verified", last.Chart.Metadata.AppVersion) {
		t.Fatal("approved revision did not receive its own binding")
	}
	assertStoredParallel(t, store, 3)
	retained, err := store.Get("oberth", 2)
	if err != nil || retained.Labels["oberth.ci/kind-revision"] != outside.Labels["oberth.ci/kind-revision"] {
		t.Fatal("recovery rewrote the outside revision's provenance", err)
	}
}

func nativeReuseValuesUpgrade(t *testing.T, store *storage.Storage, kc *deltaDeletionKube) *release.Release {
	t.Helper()
	first, err := store.Last("oberth")
	if err != nil {
		t.Fatal(err)
	}
	caps := chartutil.DefaultCapabilities.Copy()
	caps.KubeVersion = chartutil.KubeVersion{Version: "v1.30.0", Major: "1", Minor: "30"}
	config := &action.Configuration{Releases: store, KubeClient: kc, Capabilities: caps, Log: func(string, ...any) {}}
	upgrade := action.NewUpgrade(config)
	upgrade.Namespace, upgrade.ReuseValues, upgrade.Timeout = "oberth", true, time.Minute
	values := map[string]any{}
	if err = strvals.ParseInto("runs.parallel=2", values); err != nil {
		t.Fatal(err)
	}
	r := loadFixture(t, releaseFixture(t))
	result, err := upgrade.RunWithContext(t.Context(), "oberth", installer.ReleaseChartForTest(r), values)
	if err != nil {
		t.Fatal("native reuse-values upgrade failed", err)
	}
	if result.Version != 2 || result.Info.Status != release.StatusDeployed || result.Labels["oberth.ci/kind-revision"] != first.Labels["oberth.ci/kind-revision"] || result.Labels["oberth.ci/kind-revision"] == installer.RevisionBindingForTest(2, "verified", r.Version()) {
		t.Fatal("native upgrade did not preserve the stale revision binding")
	}
	assertStoredParallel(t, store, 2)
	return result
}

func reviewedReuseValuesRecovery(t *testing.T, f fixture, operation string) (*installer.Plan, func(string) error) {
	t.Helper()
	if operation == "install" {
		p := prepare(t, f)
		return p, func(approval string) error {
			_, err := f.engine.Apply(t.Context(), p, approval, nil)
			return err
		}
	}
	_, options := backupFixture(t)
	m := installer.Maintenance{Engine: f.engine, Options: options, ActiveRuns: func(context.Context, string) (int, error) { return 0, nil }}
	f.release = loadFixture(t, releaseFor(t, "v0.2.0", 1))
	p, err := m.PlanUpgrade(t.Context(), f.release, f.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	return p.Plan, func(approval string) error {
		_, err := m.Upgrade(t.Context(), p, approval, nil)
		return err
	}
}
