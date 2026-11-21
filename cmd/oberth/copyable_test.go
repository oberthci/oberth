package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
)

func TestInstallationCopyValuesStayWholeAtWidth80(t *testing.T) {
	profile := "/work/private/second/.config/oberth/development-second/installation.json"
	trust := "/work/private/second/.config/oberth/development-second/known_hosts-" + strings.Repeat("a", 64)
	key := "ssh-ed25519 " + strings.Repeat("A", 68) + " deploy-key"
	result := installer.Result{Profile: installer.Profile{AdminCert: strings.TrimSuffix(profile, "installation.json") + "admin.crt", KnownHosts: trust}, DeployPublicKey: key}
	var output bytes.Buffer
	if err := showInstallation(&output, result); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(output.String(), "\n")
	for _, value := range []string{profile, trust, key} {
		found := false
		for _, line := range lines {
			found = found || line == value
		}
		if !found {
			t.Fatal("copyable value wrapped or joined to its label", value, output.String())
		}
	}
}

func TestHelpCommandRemainsOneCopyableLine(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"help", "repo", "add"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	want := "oberth repo add <forge URL> [--known-hosts file] [--expected-host-fingerprint fingerprint] [--repin] [--upstream name]"
	if !strings.HasPrefix(out.String(), want+"\n") {
		t.Fatal("command was wrapped or lost arguments", out.String())
	}
}

func TestFlowAndRefusalCopyValuesUseTheSharedFormatter(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	path := "/work/" + strings.Repeat("long-directory/", 8) + "invitation.json"
	session := flowSession{app: app}
	if err := session.fact("invitation: %s\nnext: oberth join '%s'", path, path); err != nil {
		t.Fatal(err)
	}
	command := "oberth join '" + path + "'"
	if out.String() != "invitation:\n"+path+"\nnext:\n"+command+"\n" {
		t.Fatal(out.String())
	}
	if err := app.writeRefusal(refusalMessage{Reason: "Refused.", Remedy: "Retry.", Context: []string{"File: " + path, "Administrator: " + command}}, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diagnostic.String(), "File:\n"+path+"\nAdministrator:\n"+command+"\n") {
		t.Fatal(diagnostic.String())
	}
}
