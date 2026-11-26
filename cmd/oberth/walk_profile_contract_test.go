package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

func TestWalkInstallAndVerifySelectTheSameSavedProfile(t *testing.T) {
	cmd := exec.CommandContext(t.Context(), "python3", "-B", "testdata/walk_contract.py", "profiles", t.TempDir())
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("walk installation commands: %v\n%s", err, output)
	}
	var commands [][]string
	if err := json.Unmarshal(output, &commands); err != nil || len(commands) != 4 {
		t.Fatalf("want two install/verify pairs: %v\n%s", err, output)
	}
	for index, namespace := range []string{"oberth", "second"} {
		t.Run(namespace, func(t *testing.T) {
			checkWalkInstallationProfile(t, commands[index*2:index*2+2], namespace)
		})
	}
}

func checkWalkInstallationProfile(t *testing.T, commands [][]string, namespace string) {
	t.Helper()
	app := application{base: t.TempDir()}
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	want := "k3d-walk-1-" + namespace
	request, err := installationChoicesFor(t, "install", commands[0][1:]...)
	if err != nil || installationProfileName(request) != want {
		t.Fatal("walk installed the wrong profile", request, err)
	}
	dir := filepath.Join(app.base, installationProfileName(request))
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	writeVerificationProfile(t, dir, installer.Profile{ServerURL: "https://example.invalid", Namespace: namespace})
	verify, err := installationChoicesFor(t, "install", commands[1][1:]...)
	if err != nil || !verify.enabled("verify") {
		t.Fatal("walk omitted verification", verify, err)
	}
	session, err := app.openInstallation(verify)
	if err != nil || session.name != want || session.profile.Namespace != namespace {
		t.Fatal("walk verification did not select its installed profile", session.name, session.profile.Namespace, err)
	}
}
