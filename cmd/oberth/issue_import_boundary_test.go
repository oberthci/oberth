package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportUsesServerDecoderIncludingDryRun(t *testing.T) {
	for _, dry := range []bool{false, true} {
		t.Run(fmt.Sprint(dry), func(t *testing.T) {
			checkImportUsesServerDecoder(t, dry)
		})
	}
}

func checkImportUsesServerDecoder(t *testing.T, dry bool) {
	t.Helper()
	bindings := fake.Bindings()
	var seen apicontract.IssueImportArgs
	calls := 0
	bindings.IssueImport.Handler = apicontract.HandlerFunc[apicontract.IssueImportArgs, stateport.IssueImportResult](func(_ context.Context, _ apicontract.Caller, q apicontract.IssueImportArgs) (apicontract.Call[stateport.IssueImportResult], error) {
		calls++
		seen = q
		_, err := service.DecodeIssueImport(q)
		return nil, err
	})
	app, _, diagnostic := fixture(t, bindings)
	path := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(path, []byte("not JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	args := []string{"issue", "import", path}
	if dry {
		args = append(args, "--dry-run")
	}
	if code := app.run(t.Context(), args); code != 1 {
		t.Fatal(code)
	}
	if calls != 1 || seen.JSONL != "not JSON" || seen.DryRun != dry {
		t.Fatal("file was decoded before reaching the server", calls, seen)
	}
	if !strings.Contains(diagnostic.String(), "No issues imported.") || !strings.Contains(diagnostic.String(), "line 1") {
		t.Fatal(diagnostic.String())
	}
}

func TestIssueImportLostReplyAsksForSameFile(t *testing.T) {
	bindings := fake.Bindings()
	h := &fake.Handler[apicontract.IssueImportArgs, stateport.IssueImportResult]{Value: stateport.IssueImportResult{Count: 1}}
	bindings.IssueImport.Handler = h
	app, _, diagnostic := fixture(t, bindings)
	original := app.config.Transport
	app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) {
		reply, err := original.RoundTrip(r)
		if err != nil {
			return reply, err
		}
		if err = reply.Body.Close(); err != nil {
			return nil, err
		}
		return lostMutationReply(reply, "eof")
	})
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err := os.WriteFile(path, []byte(`{"id":1}`), 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"issue", "import", path}); code != 1 {
		t.Fatal(code)
	}
	_, _, calls := h.Observed()
	if calls != 1 || !strings.Contains(diagnostic.String(), "run the same file again") || strings.Contains(diagnostic.String(), "No issues imported.") {
		t.Fatal(calls, diagnostic.String())
	}
}

func TestIssueImportRefusesUTF8BeforeJSONTransportCanReplaceIt(t *testing.T) {
	bindings := fake.Bindings()
	h := &fake.Handler[apicontract.IssueImportArgs, stateport.IssueImportResult]{}
	bindings.IssueImport.Handler = h
	app, _, diagnostic := fixture(t, bindings)
	path := filepath.Join(t.TempDir(), "invalid.jsonl")
	if err := os.WriteFile(path, []byte{'{', '}', '\n', 255}, 0600); err != nil {
		t.Fatal(err)
	}
	if code := app.run(t.Context(), []string{"issue", "import", path}); code != 1 {
		t.Fatal(code)
	}
	_, _, calls := h.Observed()
	if calls != 0 || !strings.Contains(diagnostic.String(), "invalid UTF-8") || !strings.Contains(diagnostic.String(), "line 2") || !strings.Contains(diagnostic.String(), "No issues imported.") {
		t.Fatal(calls, diagnostic.String())
	}
}
