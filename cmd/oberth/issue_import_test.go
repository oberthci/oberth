//go:build linux

// This test imports issues through the Linux server and Git receive path.

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

// An external exporter fixture may supply the file; the same ordinary commands
// read every resulting record after import into a fresh composed installation.
func TestComposedIssueImport(t *testing.T) {
	path, batch := composedIssueFile(t)
	f := composeFixture(t, composeBinary(t))
	f.prepareCommit(t)
	f.prepareAdministrator(t)
	cancel, done := f.run(t)
	defer cancel()
	f.checkStarting(t, done)
	f.waitStarted(t, done)
	output := composedIssueCommand(t, f, []string{"issue", "import", path, "--dry-run"})
	if !strings.Contains(string(output), "dry run:") {
		t.Fatal("dry run output", string(output))
	}
	checkComposedImportDryRun(t, f)
	output = composedIssueCommand(t, f, []string{"issue", "import", path})
	if !strings.Contains(string(output), "imported:") {
		t.Fatal("import output", string(output))
	}
	for _, record := range batch.Records {
		checkComposedImportedIssue(t, f, record)
	}
	output = composedIssueCommand(t, f, []string{"issue", "import", path})
	if !strings.Contains(string(output), "already imported:") {
		t.Fatal("replay output", string(output))
	}
	audit, err := f.owners.c.store.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "issue.import", Limit: 10})
	if err != nil || len(audit) != 1 {
		t.Fatal("composed import audit", len(audit), err)
	}
	if strings.Contains(audit[0].Details, "retained text") {
		t.Fatal("content in import audit")
	}
}

func composedIssueFile(t *testing.T) (string, stateport.IssueImport) {
	t.Helper()
	data := []byte("{\"count\":1}\n" + `{"id":17,"upstream":"origin","title":"imported task","body":"retained text","state":"closed","created_at":"2021-01-02T03:04:05.123456789+02:00","closed_at":"2021-01-02T04:04:05+02:00","author":"source author","closer":"source closer","history":[{"kind":"state","at":"2021-01-02T04:04:05+02:00","author":"source closer","state":"closed"}]}` + "\n")
	if path := os.Getenv("OBERTH_TEST_ISSUE_IMPORT_FILE"); path != "" {
		var err error
		data, err = os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	batch, err := service.DecodeIssueImport(apicontract.IssueImportArgs{JSONL: string(data)})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "issues.jsonl")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}

	return path, batch
}

func checkComposedImportDryRun(t *testing.T, f *composedFixture) {
	t.Helper()
	rows, err := f.owners.c.store.ScopedIssues(t.Context(), stateport.IssueScope{Global: true}, model.IssueFilter{Limit: 50, State: "all"})
	if err != nil || len(rows) != 0 {
		t.Fatal("composed dry run wrote", err)
	}
}

func composedIssueCommand(t *testing.T, f *composedFixture, args []string) []byte {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app := application{out: &out, diagnostic: &diagnostic, env: func(key string) string {
		if key == "OBERTH_URL" {
			return f.config.Public.URL
		}
		return ""
	}, config: client.Config{Now: f.deps.clock.Now, Transport: f.client(f.config.Listen.HTTPS, f.adminCertificate).Transport}}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatal("issue command", args, code, diagnostic.String())
	}
	return out.Bytes()
}
func checkComposedImportedIssue(t *testing.T, f *composedFixture, want model.IssueImportRecord) {
	t.Helper()
	data := composedIssueCommand(t, f, []string{"issue", "get", strconv.FormatInt(want.ID, 10), "--json"})
	var got model.Issue
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err, string(data))
	}
	if got.Imported == nil {
		t.Fatal("missing imported marker")
	}
	var projection model.IssueImportRecord
	if err := json.Unmarshal(data, &projection); err != nil {
		t.Fatal(err)
	}
	actual, err := json.Marshal(projection)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	if string(actual) != string(expected) {
		t.Fatal("round-trip differs", string(actual), string(expected))
	}
}
