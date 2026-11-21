package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestBackupDryRunShowsOnlyDestinationWithoutNetworkOrFiles(t *testing.T) {
	f := &composedFixture{serverFixture: fixtureServer(t)}
	f.prepareAdministrator(t)
	app, _ := f.backupApplication(t)
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Error("dry run contacted the server")
		return nil, errors.New("unexpected snapshot request")
	})
	for _, format := range []string{"text", "json"} {
		t.Run(format, func(t *testing.T) { checkBackupPlanOutput(t, app, format) })
	}
}

func checkBackupPlanOutput(t *testing.T, app application, format string) {
	t.Helper()
	var out, diagnostic bytes.Buffer
	app.out, app.diagnostic = &out, &diagnostic
	dir := t.TempDir()
	path := filepath.Join(dir, "planned.age")
	if code := app.run(t.Context(), []string{"backup", "--out", path, "--dry-run", "--output", format}); code != 0 {
		t.Fatal("backup plan failed", code, diagnostic.String())
	}
	if format == "text" {
		if out.String() != "Planned backup: "+path+"\n" {
			t.Fatal("plan claims a completed backup", out.String())
		}
	} else {
		var value map[string]any
		if err := json.Unmarshal(out.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		if len(value) != 2 || value["dry_run"] != true || value["path"] != path {
			t.Fatal("plan contains unmeasured completion facts", value)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatal("plan created files", entries, err)
	}
	if diagnostic.Len() != 0 {
		t.Fatal("plan reports an operation", diagnostic.String())
	}
}
