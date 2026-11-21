package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestMalformedInvitationNamesItsFile(t *testing.T) {
	for _, data := range []string{"{", `{} {}`, `{"unexpected":true}`} {
		t.Run(data, func(t *testing.T) {
			app, _, diagnostic := fixture(t, fake.Bindings())
			path := filepath.Join(t.TempDir(), "invite.json")
			if err := os.Chmod(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(data), 0600); err != nil {
				t.Fatal(err)
			}
			if code := app.run(t.Context(), []string{"join", path}); code != 1 {
				t.Fatalf("malformed invitation exit=%d", code)
			}
			for _, want := range []string{"Invitation file is not valid JSON.", "Ask the administrator for a new invitation file.", "File:\n" + path} {
				if !strings.Contains(diagnostic.String(), want) {
					t.Fatalf("missing %q: %s", want, diagnostic.String())
				}
			}
			if strings.Contains(diagnostic.String(), "checkout") {
				t.Fatal("invitation incorrectly described as checkout input")
			}
		})
	}
}

func TestNativeHelpShowsFlagValueTypes(t *testing.T) {
	for _, scenario := range []struct{ name, flag string }{
		{"repo add", "--known-hosts (string):"},
		{"promote", "--repo (string):"},
		{"init", "--force (boolean):"},
		{"install", "--ssh-port (int):"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			if app.run(t.Context(), append([]string{"help"}, strings.Fields(scenario.name)...)) != 0 || !strings.Contains(out.String(), scenario.flag) {
				t.Fatalf("flag value type absent: %s / %s", out.String(), diagnostic.String())
			}
		})
	}
}
