package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
)

func TestOfflineRecordedRepairLoadsBothSignedVersions(t *testing.T) {
	f := signedMaintenanceFixture(t)
	plan := planSignedFixture(t, &f)
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("injected startup failure") }
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	if _, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("upgrade did not stop during startup")
	}
	// The review's probe finds the target still stopped, so the repair runs.
	stopped := true
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error {
		if stopped {
			stopped = false
			return errors.New("target still stopped")
		}
		return nil
	}
	bindings := fake.Bindings()
	metadata := &fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus]{}
	bindings.AdminVersion.Handler = metadata
	bindings.AdminAuditRecord.Handler = &fake.Handler[apicontract.AdminAuditRecordArgs, apicontract.Receipt]{Value: apicontract.Receipt{State: "recorded"}}
	server, _, _ := fixture(t, bindings)
	f.session.app.config = server.config
	app, _, diagnostic := f.rollbackApplication(t)
	root := t.TempDir()
	for version, directory := range map[string]string{"v0.1.0": f.previous, "v0.2.0": f.target} {
		if err := os.CopyFS(filepath.Join(root, version), os.DirFS(directory)); err != nil {
			t.Fatal(err)
		}
	}
	before := len(f.charts.Applies)
	args := []string{"upgrade", "--version", "v0.2.0", "--release-dir", root, "--no-backup", "--yes"}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("two-release offline repair failed", code, diagnostic)
	}
	if len(f.charts.Applies) != before+2 || f.charts.History.Latest.Version != "v0.2.0" {
		t.Fatal("repair did not revert A then apply B")
	}
	if _, _, calls := metadata.Observed(); calls != 0 {
		t.Fatal("recorded repair read stopped-server metadata", calls)
	}
}
