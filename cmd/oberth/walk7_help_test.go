package main

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestScheduleGroupHelp(t *testing.T) {
	for _, args := range [][]string{{"schedule", "--help"}, {"help", "schedule"}} {
		app, out, diagnostic := fixture(t, fake.Bindings())
		if code := app.run(t.Context(), args); code != 0 {
			t.Fatal(args, code, diagnostic.String())
		}
		for _, action := range []string{"list", "pause", "resume", "trigger"} {
			if !strings.Contains(out.String(), "oberth schedule "+action+":") {
				t.Fatal("missing subcommand", action, out.String())
			}
		}
	}
	app, _, _ := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"schedule", "missing", "--help"}); code == 0 {
		t.Fatal("unknown subcommand accepted")
	}
}

func TestIssueHelpDoesNotOfferFieldSelection(t *testing.T) {
	for _, args := range [][]string{{"issue", "get", "--help"}, {"issue", "get", "many", "--help"}} {
		path := strings.Join(args[:len(args)-1], " ")
		if _, advertised := commandCatalogue()[path].Flags["--fields"]; advertised {
			t.Fatal("catalogue advertises unsupported field selection", path)
		}
		app, out, diagnostic := fixture(t, fake.Bindings())
		if code := app.run(t.Context(), args); code != 0 {
			t.Fatal(code, diagnostic.String())
		}
		if strings.Contains(out.String(), "--fields") {
			t.Fatal(out.String())
		}
	}
}
