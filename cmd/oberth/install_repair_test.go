package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
)

func TestOfflineUpgradeReviewsRecordedRepairWithoutContactingStoppedServer(t *testing.T) {
	base := t.TempDir()
	engine, charts := offlineInstallEngine(t, base)
	previous, _ := signedInstallRelease(t, "v0.1.0")
	choices := installer.Choices{Context: "chosen"}
	prepared, err := engine.Prepare(t.Context(), previous, choices)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.Close()
	installed, err := engine.Apply(t.Context(), prepared, prepared.Digest(), nil)
	if err != nil {
		t.Fatal(err)
	}
	target, dir := signedInstallRelease(t, "v0.2.0")
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	maintenance := installer.Maintenance{Engine: engine, Options: options}
	plan, err := maintenance.PlanUpgrade(t.Context(), target, choices, installer.UpgradeOptions{NoBackup: true})
	if err != nil {
		t.Fatal(err)
	}
	defer plan.Plan.Close()
	engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("injected unavailable server") }
	if _, err = maintenance.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("startup failure not injected")
	}
	flags := installationFlags("upgrade")
	if err = flags.Parse([]string{"--version", "v0.2.0", "--release-dir", dir, "--no-backup", "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	requests := 0
	var output bytes.Buffer
	app := application{base: base, out: &output, diagnostic: io.Discard, config: client.Config{Now: engine.Now, Transport: testTransport(func(*http.Request) (*http.Response, error) { requests++; return nil, errors.New("server is stopped") })}}
	session := installationSession{app: app, engine: engine, profile: installed.Profile, name: "installed", choices: choices, options: stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: 99}, request: installationRequest{name: "upgrade", flags: flags}, input: bufio.NewScanner(strings.NewReader(""))}
	previews, applies := len(charts.Previews), len(charts.Applies)
	if err = session.changeInstallation(t.Context(), strings.NewReader(""), &output); err != nil {
		t.Fatal("CLI blocked recorded repair", err)
	}
	if requests != 0 || len(charts.Previews) <= previews || len(charts.Applies) != applies || !strings.Contains(output.String(), "Repair:") {
		t.Fatal("repair was not a read-only owner review", requests, output.String())
	}
}
