package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
)

func TestOfflineRecordedRepairReviewsNeverReadyUpgradeWithoutLiveMetadata(t *testing.T) {
	f := signedMaintenanceFixture(t)
	plan := planSignedFixture(t, &f)
	checks, pauses := 0, 0
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		checks++
		return installer.NewProblem("starting", nil, "target image unavailable")
	}
	f.session.engine.Pause = func(context.Context) error {
		pauses++
		if pauses == 3 {
			return context.DeadlineExceeded
		}
		return nil
	}
	maintenance := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	if _, err := maintenance.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("never-ready upgrade did not time out", err)
	}
	if checks != 3 || pauses != 3 || f.charts.History.Latest.Version != "v0.2.0" {
		t.Fatal("failure did not follow target apply and repeated readiness checks", checks, pauses)
	}
	path := filepath.Join(f.session.app.base, "installed", "rollback.json")
	before := neverReadyRepairRecord(t, path)
	requests := 0
	f.session.app.config = client.Config{Now: f.session.engine.Now, Transport: testTransport(func(*http.Request) (*http.Response, error) {
		requests++
		return nil, errors.New("target never became ready")
	})}
	app, out, diagnostic := f.rollbackApplication(t)
	applies := len(f.charts.Applies)
	args := []string{"upgrade", "--version", "v0.2.0", "--release-dir", f.target, "--no-backup", "--dry-run"}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("recorded repair required the never-ready server", code, diagnostic)
	}
	// One bounded readiness probe decides between completing the record and repair.
	if requests != 0 || checks != 4 || len(f.charts.Applies) != applies || !strings.Contains(out.String(), "Repair:") {
		t.Fatal("repair review contacted live metadata or applied changes", requests, checks, out)
	}
	if after := neverReadyRepairRecord(t, path); !bytes.Equal(before, after) {
		t.Fatal("repair review rewrote pending recovery evidence")
	}
}

func neverReadyRepairRecord(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var record installer.Record
	if err := json.Unmarshal(raw, &record); err != nil || record.State != "pending" || record.Source.Version != "v0.1.0" || record.TargetVersion != "v0.2.0" {
		t.Fatal("missing genuine pending A-to-B record", record, err)
	}
	return raw
}
