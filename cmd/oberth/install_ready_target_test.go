package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

// The walk's shape: the readiness wait expired, the target revision stayed
// deployed and the server then became ready on the target version.
func TestReadyTargetAfterExpiredWaitCompletesTheRecordWithoutChanges(t *testing.T) {
	f, app, out, diagnostic := expiredReadinessUpgrade(t)
	f.session.engine.CheckEndpoint = func(_ context.Context, p installer.Profile) error {
		if p.ServerVersion != f.charts.History.Latest.Version {
			return errors.New("server serves another version")
		}
		return nil
	}
	history, applies := f.charts.History, len(f.charts.Applies)
	args := []string{"upgrade", "--version", "v0.2.0", "--release-dir", bothReleases(t, f), "--no-backup"}
	if code := app.run(t.Context(), append(args, "--dry-run")); code != 0 {
		t.Fatal("ready target review failed", code, diagnostic)
	}
	if text := out.String(); !strings.Contains(text, "Recovery: complete the ready upgrade; no chart or database changes.") || strings.Contains(text, "Repair:") {
		t.Fatal("review does not say it completes the record", text)
	}
	out.Reset()
	if code := app.run(t.Context(), append(args, "--yes")); code != 0 {
		t.Fatal("ready target completion failed", code, diagnostic)
	}
	if len(f.charts.Applies) != applies || !reflect.DeepEqual(history, f.charts.History) {
		t.Fatal("completion rolled back or applied again", len(f.charts.Applies)-applies)
	}
	if record := upgradeRecord(t, app); record.State != "completed" || record.TargetRevision != history.Latest.Number {
		t.Fatal("record not completed", record.State)
	}
	if !strings.Contains(out.String(), "Upgrade completed; the recorded release is ready.") {
		t.Fatal("completion not reported", out)
	}
}

func TestTargetNotReadyAfterExpiredWaitIsStillRepaired(t *testing.T) {
	f, app, out, diagnostic := expiredReadinessUpgrade(t)
	// The interrupted revision never serves; the rolled-back and reapplied ones do.
	stopped := f.charts.History.Latest.Number
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		if f.charts.History.Latest.Number == stopped {
			return installer.NewProblem("starting", nil, "")
		}
		return nil
	}
	applies := len(f.charts.Applies)
	args := []string{"upgrade", "--version", "v0.2.0", "--release-dir", bothReleases(t, f), "--no-backup"}
	if code := app.run(t.Context(), append(args, "--dry-run")); code != 0 || !strings.Contains(out.String(), "Repair: roll back the interrupted change, then apply this release.") {
		t.Fatal("stopped target is not reviewed for repair", code, out, diagnostic)
	}
	if code := app.run(t.Context(), append(args, "--yes")); code != 0 {
		t.Fatal("repair failed", code, diagnostic)
	}
	if len(f.charts.Applies) != applies+2 || f.charts.History.Latest.Version != "v0.2.0" || upgradeRecord(t, app).State != "completed" {
		t.Fatal("stopped target was not rolled back and applied again", len(f.charts.Applies)-applies)
	}
}

func TestReadyTargetThatStopsAfterReviewAsksForAnotherReview(t *testing.T) {
	f, app, _, _ := expiredReadinessUpgrade(t)
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return nil }
	plan := planSignedFixture(t, &f)
	if !strings.Contains(strings.Join(plan.Plan.Review(), "\n"), "Recovery: complete the ready upgrade") {
		t.Fatal("ready target was not reviewed for completion")
	}
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("stopped after review") }
	history, applies := f.charts.History, len(f.charts.Applies)
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	_, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil)
	var refusal *installer.Problem
	if !errors.As(err, &refusal) || refusal.Code != "target" || refusal.Detail != "The recorded release is no longer ready." {
		t.Fatal("stopped target was not sent back to review", err)
	}
	if len(f.charts.Applies) != applies || !reflect.DeepEqual(history, f.charts.History) || upgradeRecord(t, app).State != "pending" {
		t.Fatal("refused completion changed the installation")
	}
}

func expiredReadinessUpgrade(t *testing.T) (signedMaintenance, application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	f := pendingBudgetUpgrade(t)
	bindings := fake.Bindings()
	bindings.AdminVersion.Handler = &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{Value: apicontract.SystemStatus{SchemaVersion: store.Schema, Protocol: stateport.DefaultProtocol()}}
	bindings.AdminAuditRecord.Handler = &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: "recorded"}}
	server, _, _ := fixture(t, bindings)
	f.session.app.config = server.config
	// The fake server authenticates its bearer; the profile's certificate stands in for it.
	f.session.app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		r.Header.Set("Authorization", "Bearer valid")
		return server.config.Transport.RoundTrip(r)
	})
	app, out, diagnostic := f.rollbackApplication(t)
	latest := f.charts.History.Latest
	if record := upgradeRecord(t, app); record.State != "pending" || latest.Version != "v0.2.0" || latest.Status != "deployed" || latest.Number != record.TargetRevision {
		t.Fatal("fixture is not an expired wait on a deployed target", record.State, latest)
	}
	return f, app, out, diagnostic
}

func bothReleases(t *testing.T, f signedMaintenance) string {
	t.Helper()
	root := t.TempDir()
	for version, directory := range map[string]string{"v0.1.0": f.previous, "v0.2.0": f.target} {
		if err := os.CopyFS(filepath.Join(root, version), os.DirFS(directory)); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func upgradeRecord(t *testing.T, app application) installer.Record {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(app.base, "installed", "rollback.json"))
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err = json.Unmarshal(raw, &record); err != nil {
		t.Fatal(err)
	}
	return record
}
