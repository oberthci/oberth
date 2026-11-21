package main

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportCompleteFrameIsRefusedBeforeSend(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	calls := 0
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) { calls++; return nil, nil })
	path := filepath.Join(t.TempDir(), "frame.jsonl")
	data := strings.Repeat(" ", apicontract.RequestBytes-60)
	raw, _ := json.Marshal(apicontract.IssueImportArgs{JSONL: data})
	if len(raw) > apicontract.RequestBytes {
		t.Fatal("arguments already exceed the limit")
	}
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"issue", "import", path}) != 1 || calls != 0 {
		t.Fatal("oversized frame reached transport", calls)
	}
	if !strings.Contains(diagnostic.String(), "1048576") || !strings.Contains(diagnostic.String(), "No issues imported.") || strings.Contains(diagnostic.String(), "unknown") || strings.Contains(diagnostic.String(), "uncertain") {
		t.Fatal(diagnostic.String())
	}
}

func TestIssueImportDefiniteServerRefusalStaysDefinite(t *testing.T) {
	bindings := fake.Bindings()
	bindings.IssueImport.Handler = apicontract.HandlerFunc[apicontract.IssueImportArgs, stateport.IssueImportResult](func(context.Context, apicontract.Caller, apicontract.IssueImportArgs) (apicontract.Call[stateport.IssueImportResult], error) {
		return nil, publicerr.New(publicerr.InvalidInput, "This import was refused.", "Check the file and retry.", nil)
	})
	app, _, diagnostic := fixture(t, bindings)
	path := filepath.Join(t.TempDir(), "refused.jsonl")
	if err := os.WriteFile(path, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"issue", "import", path}) != 1 {
		t.Fatal("refusal succeeded")
	}
	text := diagnostic.String()
	if !strings.Contains(text, "This import was refused.") || !strings.Contains(text, "No issues imported.") || strings.Contains(text, "unknown") || strings.Contains(text, "uncertain") {
		t.Fatal(text)
	}
}
