package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBackupPublicationWarningsKeepSuccessAndOneRenderer(t *testing.T) {
	for _, code := range []string{"backup-directory-sync", "backup-cleanup"} {
		for _, format := range []string{"text", "json"} {
			t.Run(code+"/"+format, func(t *testing.T) { checkBackupWarning(t, code, format) })
		}
	}
}

func checkBackupWarning(t *testing.T, code, format string) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	path := filepath.Join(t.TempDir(), "complete.age")
	archive := installer.Archive{Path: path, SHA256: strings.Repeat("a", 64), StoreSnapshotHint: "Separate store snapshot hint."}
	problem := installer.NewProblem(code, errors.New("storage diagnostic"), path+"\nstorage diagnostic")
	app.install = func(_ context.Context, r installationRequest, _ io.Reader, w io.Writer) error {
		return (installationSession{app: app, request: r}).backupResult(w, archive, problem)
	}
	if code := app.run(t.Context(), []string{"backup", "--database-only", "--out", path, "--output", format}); code != 0 {
		t.Fatal("published backup reported failure", code, diagnostic.String())
	}
	var want bytes.Buffer
	rendered := app
	rendered.diagnostic = &want
	rendered.failed(installationFailure(problem))
	if diagnostic.String() != want.String() || !strings.Contains(diagnostic.String(), "Backup is published") {
		t.Fatal("warning bypassed shared refusal renderer", diagnostic.String())
	}
	if format == "json" {
		var result struct {
			installer.Archive
			Warning json.RawMessage
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || result.Path != path || result.SHA256 != archive.SHA256 {
			t.Fatal("publication warning lost completed JSON facts", err)
		}
		expected, err := json.Marshal(publicerr.Classify(installationFailure(problem)))
		if err != nil || !bytes.Equal(result.Warning, expected) {
			t.Fatal("JSON warning differs from the public diagnostic", string(result.Warning), err)
		}
	} else if !strings.Contains(out.String(), archive.StoreSnapshotHint) || !strings.Contains(out.String(), path) {
		t.Fatal("publication warning lost result or hint", out.String())
	}
}

func TestBackupFailuresUseTheSharedRefusalAndStayFailed(t *testing.T) {
	for _, code := range []string{"restore-exists", "backup-hardlink", "backup-publish", "backup-destination", "backup"} {
		t.Run(code, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			path := filepath.Join(t.TempDir(), "unpublished.age")
			problem := installer.NewProblem(code, errors.New("snapshot source refused"), path+"\nsnapshot source refused")
			app.install = func(_ context.Context, r installationRequest, _ io.Reader, w io.Writer) error {
				return (installationSession{app: app, request: r}).backupResult(w, installer.Archive{}, problem)
			}
			if code := app.run(t.Context(), []string{"backup", "--database-only", "--out", path}); code != 1 || out.Len() != 0 {
				t.Fatal("failed backup reported completion", code, out.String())
			}
			var want bytes.Buffer
			rendered := app
			rendered.diagnostic = &want
			rendered.failed(installationFailure(problem))
			if diagnostic.String() != want.String() || !strings.Contains(diagnostic.String(), path) || !strings.Contains(diagnostic.String(), "snapshot source refused") {
				t.Fatal("failed backup lost shared rendering, path or source cause", diagnostic.String())
			}
		})
	}
}

func TestBackupJSONOmitsWarningOnCleanResult(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		archive := installer.Archive{Path: "backup.age", DryRun: dryRun}
		app, output, diagnostic := fixture(t, fake.Bindings())
		app.install = func(_ context.Context, r installationRequest, _ io.Reader, w io.Writer) error {
			return (installationSession{app: app, request: r}).backupResult(w, archive, nil)
		}
		if code := app.run(t.Context(), []string{"backup", "--database-only", "--out", archive.Path, "--output", "json"}); code != 0 {
			t.Fatal("clean JSON backup failed", code, diagnostic.String())
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if _, exists := result["warning"]; exists {
			t.Fatal("clean backup invented a warning")
		}
	}
}

// A database-only archive cannot be restored, so it must never start retention.
func TestDatabaseOnlyBackupRefusesRetention(t *testing.T) {
	for _, args := range [][]string{{"--keep", "1"}, {"--keep=7"}, {"--directory", "backups"}, {"--directory", "backups", "--keep", "2"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
				t.Fatal("database-only retention reached the backup")
				return nil
			}
			if code := app.run(t.Context(), append([]string{"backup", "--database-only"}, args...)); code != 2 || out.Len() != 0 {
				t.Fatal("database-only retention was not refused", code, out.String())
			}
			if !strings.Contains(diagnostic.String(), "Retention needs a full backup.") || !strings.Contains(diagnostic.String(), "run a full backup to apply") {
				t.Fatal("refusal lost its reason or remedy", diagnostic.String())
			}
		})
	}
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error { return nil }
	if code := app.run(t.Context(), []string{"backup", "--database-only", "--out", "copy.age"}); code != 0 {
		t.Fatal("plain database-only backup was refused", code, diagnostic.String())
	}
}
