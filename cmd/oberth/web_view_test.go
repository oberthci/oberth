package main

import (
	"bytes"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
)

// Install tells the administrator how to open the web view: trust the printed
// Server CA and sign in with a bearer from the Browser access guide's command.
func TestInstallResultSaysHowToOpenTheWebView(t *testing.T) {
	var out bytes.Buffer
	if err := showInstallation(&out, installer.Result{Profile: installer.Profile{CAFile: "/profile/ca.crt"}}); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	if !strings.Contains(text, "Server CA: /profile/ca.crt") || !strings.Contains(text, strings.Join(strings.Fields(installWebView()), " ")) {
		t.Fatal("install omits the web view", out.String())
	}
	for _, text := range []string{installWebView(), joinedWebView("", []string{"origin"})} {
		lines := strings.Split(text, "\n")
		if len(lines) != 2 || len([]rune(lines[0])) > 120 || !strings.HasPrefix(lines[1], "oberth uplink add ") {
			t.Fatal("web view is not one short line and its command", text)
		}
		for _, guide := range []string{"--identity <unused-name> ", " --ttl 1h ", " --bearer-file <new-file>"} {
			if !strings.Contains(lines[1], guide) {
				t.Fatal("web view command differs from the Browser access guide", guide, lines[1])
			}
		}
	}
	if !strings.Contains(joinedWebView("", []string{"origin"}), `--scope '["origin"]'`) {
		t.Fatal("join's bearer command is not limited to the joined scope")
	}
}

// The printed command, with its placeholders filled, is one the current CLI
// accepts and sends with the intended scope and lifetime.
func TestWebViewCommandRunsAsPrinted(t *testing.T) {
	b := credentialBinding("uplink_add", "")
	handler := b.UplinkAdd.Handler.(*fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity])
	app, _, diagnostic := fixture(t, b)
	path := filepath.Join(filepath.Dir(app.env("OBERTH_TOKEN_FILE")), "browser-bearer")
	command := strings.Fields(strings.Split(joinedWebView("", []string{"origin"}), "\n")[1])
	for i, word := range command {
		command[i] = strings.NewReplacer("<unused-name>", "agent-browser", "<new-file>", path, "'", "").Replace(word)
	}
	if code := app.run(t.Context(), command[1:]); code != 0 {
		t.Fatal("printed web view command refused", code, diagnostic.String())
	}
	args, _, calls := handler.Observed()
	if calls != 1 || args[0].Identity != "agent-browser" || !slices.Equal(args[0].Scope, []string{"origin"}) || args[0].TTL != "1h" || len(args[0].BearerDigest) != 64 {
		t.Fatal("printed command sent another request", args, calls)
	}
}

// Only a new installation names the join step; an update's identities exist.
func TestAppliedUpdatePrintsNoFirstRunStep(t *testing.T) {
	for _, updated := range []bool{false, true} {
		var out bytes.Buffer
		if err := (application{}).showApplied(&out, installer.Result{Next: "next", Updated: updated}); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "oberth join") == updated {
			t.Fatal("join step shown for the wrong apply", updated, out.String())
		}
	}
}
