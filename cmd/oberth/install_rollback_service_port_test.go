package main

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

// The published chart stays byte-for-byte intact; only its release envelope
// uses the test signer. The command dispatches through the real rollback path.
func TestRollbackCommandServicePortRemedyWithArchivedPreviousChart(t *testing.T) {
	f, previous := archivedServicePortMaintenance(t)
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	upgradeServicePortFixture(t, m, servicePortUpgrade{f.target, "v0.30.18", 443})
	app, out, diagnostic := f.rollbackApplication(t)
	path := filepath.Join(app.base, "installed", "rollback.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	history, applies := f.charts.History, len(f.charts.Applies)
	code := app.run(t.Context(), []string{"rollback", "--release-dir", f.previous, "--yes"})
	after, err := os.ReadFile(path)
	if code != 1 || err != nil || !bytes.Equal(before, after) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
		t.Fatal("refused rollback changed the installation", code, err, out, diagnostic)
	}
	assertRollbackServicePortRemedy(t, diagnostic.String())
	// Follow the remedy: explicitly upgrade to the earlier release at its fixed port.
	upgradeServicePortFixture(t, m, servicePortUpgrade{f.previous, previous.Version(), 8443})
	last := f.charts.Applies[len(f.charts.Applies)-1]
	if last.Chart.Metadata.AppVersion != "v0.30.17" || f.charts.History.Latest.Values["servicePort"] != nil {
		t.Fatal("remedy did not apply the archived chart with its declared values")
	}
	files, err := installer.Render(last.Chart, last.Values, last.Namespace)
	if err != nil || !strings.Contains(files["oberth/templates/server.yaml"], "port: 8443") {
		t.Fatal("remedy did not restore the fixed Service port", err, files)
	}
}

func assertRollbackServicePortRemedy(t *testing.T, diagnostic string) {
	t.Helper()
	for _, text := range []string{"Rollback needs the default Service port.", "oberth upgrade --version <earlier-version> --service-port 8443", "HTTPS Service port: 443", "v0.30.17"} {
		if !strings.Contains(strings.Join(strings.Fields(diagnostic), " "), text) {
			t.Fatal("rollback lost its service-port remedy", text, diagnostic)
		}
	}
}

func archivedServicePortMaintenance(t *testing.T) (signedMaintenance, installer.Release) {
	t.Helper()
	body, err := os.ReadFile("../../internal/installer/testdata/oberth-v0.30.17.tgz")
	if err != nil || fmt.Sprintf("%x", sha256.Sum256(body)) != "8179ba3ff6047727084e5afc463e2fc345adbc4e98ba15037b626062b7b47158" {
		t.Fatal("published previous chart checksum differs", err)
	}
	previous, directory := signedInstallArchive(t, "v0.30.17", body)
	base := t.TempDir()
	engine, charts := offlineInstallEngine(t, base)
	choices := installer.Choices{Context: "chosen"}
	plan, err := engine.Prepare(t.Context(), previous, choices)
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Close()
	installed, err := engine.Apply(t.Context(), plan, plan.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	_, target := signedInstallRelease(t, "v0.30.18")
	return signedMaintenance{session: installationSession{engine: engine, app: application{base: base}, choices: choices, profile: installed.Profile, name: "installed", options: stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}}, charts: charts, previous: directory, target: target}, previous
}

type servicePortUpgrade struct {
	directory, version string
	port               int
}

func upgradeServicePortFixture(t *testing.T, m installer.Maintenance, upgrade servicePortUpgrade) {
	t.Helper()
	flags := installationFlags("upgrade")
	if err := flags.Parse([]string{"--version", upgrade.version, "--release-dir", upgrade.directory, "--service-port", fmt.Sprint(upgrade.port)}); err != nil {
		t.Fatal(err)
	}
	r := installationRequest{name: "upgrade", flags: flags}
	release, err := installationRelease(t.Context(), r)
	if err != nil {
		t.Fatal(err)
	}
	choices := r.choices()
	choices.Context = m.Engine.Cluster.Context
	plan, err := m.PlanUpgrade(t.Context(), release, choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	if _, err = m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err != nil {
		t.Fatal(err)
	}
}
