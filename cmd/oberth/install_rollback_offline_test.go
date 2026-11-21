package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	chartfake "github.com/oberthci/oberth/internal/installer/fake"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

type signedMaintenance struct {
	session          installationSession
	charts           *chartfake.Chart
	previous, target string
}

func signedMaintenanceFixture(t *testing.T) signedMaintenance {
	t.Helper()
	base := t.TempDir()
	engine, charts := offlineInstallEngine(t, base)
	earlier, previous := signedInstallRelease(t, "v0.1.0")
	choices := installer.Choices{Context: "chosen"}
	plan, err := engine.Prepare(t.Context(), earlier, choices)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	installed, err := engine.Apply(t.Context(), plan, plan.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, target := signedInstallRelease(t, "v0.2.0")
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	return signedMaintenance{session: installationSession{engine: engine, app: application{base: base}, choices: choices, profile: installed.Profile, name: "installed", options: options}, charts: charts, previous: previous, target: target}
}

func TestOfflineRollbackCommandUsesVerifiedRecordedSource(t *testing.T) {
	for _, wrong := range []bool{false, true} {
		t.Run(map[bool]string{false: "recorded version", true: "other signed version"}[wrong], func(t *testing.T) {
			offlineRollbackCase(t, wrong)
		})
	}
}

func offlineRollbackCase(t *testing.T, wrong bool) {
	t.Helper()
	f := signedMaintenanceFixture(t)
	upgradeSignedFixture(t, &f)
	app, out, diagnostic := f.rollbackApplication(t)
	directory := f.previous
	if wrong {
		directory = f.target
	}
	path := filepath.Join(app.base, "installed", "rollback.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	history, applies := f.charts.History, len(f.charts.Applies)
	code := app.run(t.Context(), []string{"rollback", "--release-dir", directory, "--yes"})
	if !wrong {
		if code != 0 || f.charts.History.Latest.Version != "v0.1.0" {
			t.Fatal("offline rollback failed", code, out, diagnostic)
		}
		if want := fmt.Sprintf("Rolled back to v0.1.0, revision %d.\n", f.charts.History.Latest.Number); out.String() != want {
			t.Fatal("rollback does not say what it did", out)
		}
		return
	}
	after, err := os.ReadFile(path)
	if code != 1 || err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
		t.Fatal("wrong release changed state", code, err)
	}
	assertRequiredRollbackVersion(t, diagnostic.String())
}

func upgradeSignedFixture(t *testing.T, f *signedMaintenance) {
	t.Helper()
	plan := planSignedFixture(t, f)
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	if _, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
}

func planSignedFixture(t *testing.T, f *signedMaintenance) *installer.UpgradePlan {
	t.Helper()
	flags := installationFlags("upgrade")
	if err := flags.Parse([]string{"--version", "v0.2.0", "--release-dir", f.target}); err != nil {
		t.Fatal(err)
	}
	release, err := installationRelease(t.Context(), installationRequest{name: "upgrade", flags: flags})
	if err != nil {
		t.Fatal(err)
	}
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	plan, err := m.PlanUpgrade(t.Context(), release, f.session.choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(plan.Plan.Close)
	return plan
}

func (f signedMaintenance) rollbackApplication(t *testing.T) (application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app := f.session.app
	app.in, app.out, app.diagnostic = strings.NewReader(""), &out, &diagnostic
	if app.config.Transport == nil {
		// Only the best-effort audit receipt may reach a server; it is refused here.
		app.config = client.Config{Now: f.session.engine.Now, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			raw, err := io.ReadAll(r.Body)
			if err != nil || !bytes.Contains(raw, []byte(`"admin_audit_record"`)) {
				t.Fatal("offline rollback contacted a server", err)
			}
			return nil, io.EOF
		})}
	}
	app.install = func(ctx context.Context, request installationRequest, in io.Reader, output io.Writer) error {
		session := f.session
		session.app, session.request, session.input = app, request, bufio.NewScanner(in)
		return session.dispatch(ctx, in, output)
	}
	return app, &out, &diagnostic
}

func assertRequiredRollbackVersion(t *testing.T, diagnostic string) {
	t.Helper()
	for _, text := range []string{"Required rollback version:\nv0.1.0", "Release version differs from the requested version."} {
		if !strings.Contains(diagnostic, text) {
			t.Fatal("canonical refusal or required version missing", diagnostic)
		}
	}
}
