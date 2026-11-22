package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
)

// Install tells the administrator how to open the web view: trust the printed
// Server CA and sign in with a bearer from the exact uplink add command
// (TestUplinkAddBearerFileNeedsNoSSHOption runs that command shape).
func TestInstallResultSaysHowToOpenTheWebView(t *testing.T) {
	var out bytes.Buffer
	if err := showInstallation(&out, installer.Result{Profile: installer.Profile{CAFile: "/profile/ca.crt"}}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(text, "Server CA: /profile/ca.crt") || !strings.Contains(text, installWebView) {
		t.Fatal("install omits the web view", out.String())
	}
	for _, line := range []string{installWebView, joinedWebView("")} {
		if !strings.Contains(line, webBearerCommand) || len([]rune(line)) > 120 {
			t.Fatal("web view line lacks the command or exceeds 120 characters", len([]rune(line)), line)
		}
	}
}
