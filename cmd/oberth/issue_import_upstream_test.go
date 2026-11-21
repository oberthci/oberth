package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportUpstreamMappingReachesServerWithOriginalFile(t *testing.T) {
	bindings := fake.Bindings()
	h := &fake.Handler[apicontract.IssueImportArgs, stateport.IssueImportResult]{Value: stateport.IssueImportResult{Count: 1, DryRun: true}}
	bindings.IssueImport.Handler = h
	app, _, diagnostic := fixture(t, bindings)
	path := filepath.Join(t.TempDir(), "source.jsonl")
	source := "{\"count\":1}\n{\"id\":17,\"upstream\":\"source-name\"}\n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"issue", "import", path, "--dry-run", "--upstream", "local"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	inputs, _, calls := h.Observed()
	if len(inputs) != 1 {
		t.Fatal("wrong preparation count", len(inputs))
	}
	got := inputs[0]
	if calls != 1 || got.JSONL != source || got.Upstream != "local" || !got.DryRun {
		t.Fatal("mapping or original file did not reach server", calls, got.Upstream)
	}
}

func TestIssueImportHelpExplainsExplicitUpstreamMapping(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"issue", "import", "--help"}); code != 0 {
		t.Fatal(code, diagnostic.String())
	}
	if !strings.Contains(out.String(), "--upstream") || !strings.Contains(out.String(), "Map every source upstream") {
		t.Fatal(out.String())
	}
}
