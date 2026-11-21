package main

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestInstallationOutputRefusalKeepsReasonBeforeDispatch(t *testing.T) {
	for _, test := range []struct {
		command string
		output  string
		reason  string
	}{
		{"install", "yaml", "--output must be text or json."},
		{"upgrade", "json", "--output json requires --dry-run for an installation review."},
	} {
		t.Run(test.command, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
				t.Fatal("invalid output option reached installation dispatch")
				return nil
			}
			if code := app.run(t.Context(), []string{test.command, "--output=" + test.output}); code != 2 {
				t.Fatal("output refusal exit code", code)
			}
			if !strings.Contains(diagnostic.String(), test.reason) || strings.Contains(diagnostic.String(), "invalid command arguments") {
				t.Fatal("output refusal lost its specific reason", diagnostic.String())
			}
			if out.Len() != 0 {
				t.Fatal("output refusal wrote to standard output", out.String())
			}
		})
	}
}

func TestStoreSetupDoesNotOfferAnUnimplementedPrintOption(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
		t.Fatal("removed print flag reached owner")
		return nil
	}
	if code := app.run(t.Context(), []string{"secretstore", "setup", "--print"}); code != 2 {
		t.Fatal("print flag accepted", code)
	}
	if app.run(t.Context(), []string{"help", "secretstore", "setup"}) != 0 || strings.Contains(out.String(), "--print") {
		t.Fatal("print flag advertised", out.String(), diagnostic.String())
	}
	if _, ok := commandCatalogue()["secretstore setup"].Flags["--print"]; ok {
		t.Fatal("catalogue advertises print")
	}
}

func TestUpgradeAcceptsSignedInputsAndRefusesLocalChartBeforeOwner(t *testing.T) {
	for _, arg := range []string{"--chart", "--chart=local"} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
			t.Fatal("upgrade chart reached owner")
			return nil
		}
		if app.run(t.Context(), []string{"upgrade", arg}) != 2 || !strings.Contains(diagnostic.String(), "oberth install --chart") {
			t.Fatal("local chart remedy missing", diagnostic.String())
		}
		assertUpgradeChartRefusal(t, diagnostic.String())
	}
	app, out, diagnostic := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"help", "upgrade"}) != 0 || strings.Contains(out.String(), "--chart") || !strings.Contains(out.String(), "signed release") {
		t.Fatal("upgrade help is misleading", out.String(), diagnostic.String())
	}
	if _, ok := commandCatalogue()["upgrade"].Flags["--chart"]; ok {
		t.Fatal("upgrade chart catalogued")
	}
	if _, ok := commandCatalogue()["install"].Flags["--chart"]; !ok {
		t.Fatal("local install lost chart input")
	}
}

func assertUpgradeChartRefusal(t *testing.T, diagnostic string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(diagnostic), "\n")
	if len(lines) != 2 || lines[0] != "Upgrades take signed releases." || lines[1] != "Use oberth install --chart <directory> for a local chart." {
		t.Fatal("chart refusal repeats or obscures its remedy", diagnostic)
	}
	if len([]rune(lines[0])) > 80 || len([]rune(lines[1])) > 120 {
		t.Fatal("chart refusal exceeds reason/remedy budgets")
	}
}
