package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestValidationParserDetailHasOneRenderer(t *testing.T) {
	for _, detail := range []bool{false, true} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.directory = t.TempDir()
		if err := os.WriteFile(filepath.Join(app.directory, "bad.yaml"), []byte("apiVersion: ["), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{"validate", "bad.yaml"}
		if detail {
			args = append(args, "--detail")
		}
		if code := app.run(t.Context(), args); code != 1 {
			t.Fatal("invalid YAML accepted", code)
		}
		text := diagnostic.String()
		if strings.Count(text, "Workflow admission refused.") != 1 {
			t.Fatal("refusal duplicated", text)
		}
		if detail && !strings.Contains(text, "did not find expected node content") {
			t.Fatal("parser diagnostic lost", text)
		}
		if !detail && !strings.Contains(text, "--detail") {
			t.Fatal("parser detail silently omitted", text)
		}
	}
}
func TestHelpTranslatesOnlyExplicitReferencesOnce(t *testing.T) {
	for input, want := range map[string]string{"the condition that ends this wait": "the condition that ends this wait", "instead of waiting for an answer": "instead of waiting for an answer", "use run_wait or admin_version": "use run wait or tool admin_version", "prefix_run_wait_suffix": "prefix_run_wait_suffix"} {
		if got := cliWords(input); got != want {
			t.Errorf("%q -> %q; want %q", input, got, want)
		}
	}
}
func TestSharedCatalogueOwnsHumanNames(t *testing.T) {
	rows := commandCatalogue()
	for _, name := range []string{"admin readiness", "admin version", "admin access list", "publish retry", "promotion list", "release wif list", "release identity list", "release identity grant", "release identity revoke"} {
		if _, ok := rows[name]; ok {
			t.Error("internal or duplicate human name", name)
		}
	}
	for _, name := range []string{"system status", "publication retry", "promote list"} {
		if _, ok := rows[name]; !ok {
			t.Error("human group missing", name)
		}
	}
	for _, row := range commands() {
		if row.path != "" && row.path != row.definition.Command {
			t.Error("command path overrides shared catalogue", row.path)
		}
		if _, _, found := findCommand(commands(), []string{"tool", row.definition.Name}); !found {
			t.Error("raw route missing", row.definition.Name)
		}
	}
}
func TestUnknownCommandIsSentenceCaseWithOneHelpRemedy(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"unlisted-action"}) != 2 {
		t.Fatal("unknown action accepted")
	}
	text := diagnostic.String()
	if !strings.Contains(text, "Unknown command") || strings.Count(strings.ToLower(text), "oberth help") != 1 {
		t.Fatal("unknown command remedy duplicated", text)
	}
}

func TestReleaseIdentityConfigurationHasNoCommandOrToolAlias(t *testing.T) {
	for _, verb := range []string{"grant", "list", "revoke"} {
		for _, args := range [][]string{{"release", "identity", verb}, {"tool", "release_wif_" + verb, "{}"}} {
			app, _, diagnostic := fixture(t, fake.Bindings())
			if code := app.run(t.Context(), args); code != 2 {
				t.Fatal("removed release configuration is callable", args, code, diagnostic.String())
			}
		}
	}
}
