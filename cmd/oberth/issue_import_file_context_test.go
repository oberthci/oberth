package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportEveryErrorCarriesSelectedFile(t *testing.T) {
	for _, shape := range []string{"missing", "directory", "file_size", "utf8", "server"} {
		t.Run(shape, func(t *testing.T) { checkImportFileContext(t, shape) })
	}
}

func checkImportFileContext(t *testing.T, shape string) {
	t.Helper()
	bindings := fake.Bindings()
	bindings.IssueImport.Handler = apicontract.HandlerFunc[apicontract.IssueImportArgs, stateport.IssueImportResult](func(_ context.Context, _ apicontract.Caller, q apicontract.IssueImportArgs) (apicontract.Call[stateport.IssueImportResult], error) {
		_, err := service.DecodeIssueImport(q)
		return nil, err
	})
	app, _, diagnostic := fixture(t, bindings)
	path := filepath.Join(t.TempDir(), shape+".jsonl")
	writeImportContextFile(t, path, shape)
	if app.run(t.Context(), []string{"issue", "import", path}) != 1 || !strings.Contains(diagnostic.String(), "File:\n"+path+"\n") {
		t.Fatal("human refusal omits selected file", diagnostic.String())
	}
	session := &flowSession{app: app, ctx: t.Context()}
	defer session.close()
	err := importIssues(session, []string{path})
	raw, encodeErr := json.Marshal(publicerr.Classify(err))
	if err == nil || encodeErr != nil || !strings.Contains(string(raw), "File: "+path) {
		t.Fatal("structured refusal omits selected file", string(raw), err, encodeErr)
	}
	if shape == "server" && (!strings.Contains(string(raw), "line 2, issue 42") || !strings.Contains(string(raw), "No issues imported.")) {
		t.Fatal(string(raw))
	}
}

func writeImportContextFile(t *testing.T, path, shape string) {
	t.Helper()
	switch shape {
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	case "file_size", "utf8", "server":
		data := []byte("{\"count\":1}\n{\"id\":42,\"title\":\"copy\",\"state\":\"open\",\"kind\":\"ci\",\"created_at\":\"2020-01-01T00:00:00Z\"}")
		if shape == "file_size" {
			data = []byte(strings.Repeat("x", apicontract.RequestBytes+1))
		}
		if shape == "utf8" {
			data = []byte{'{', '}', '\n', 255}
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
}
