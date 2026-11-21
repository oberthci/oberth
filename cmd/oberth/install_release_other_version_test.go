package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/releaseverify"
)

func TestOfflineOtherFlatVersionKeepsLookupGuidanceWithoutMutation(t *testing.T) {
	f := signedMaintenanceFixture(t)
	root := filepath.Join(t.TempDir(), strings.Repeat("offline-release-", 12))
	if err := os.CopyFS(root, os.DirFS(f.target)); err != nil {
		t.Fatal(err)
	}
	_, err := installationRelease(t.Context(), offlineLookupRequest(t, root, "v0.1.0"))
	if !errors.Is(err, releaseverify.Version) {
		t.Fatal("other signed version bypassed canonical refusal", err)
	}
	var problem *publicerr.Error
	if !errors.As(installationFailure(err), &problem) {
		t.Fatal("version refusal lost its public diagnostic", err)
	}
	if len([]rune(problem.Reason())) > 80 || len([]rune(problem.Remedy())) > 120 {
		t.Fatal("lookup diagnostic exceeds message budgets", problem)
	}
	assertOtherFlatVersionGuidance(t, strings.Join(append(problem.Context, problem.Reason(), problem.Remedy()), "\n"), root)
	plan := planSignedFixture(t, &f)
	f.session.engine.CheckEndpoint = func(context.Context, installer.Profile) error { return errors.New("target unavailable") }
	m := installer.Maintenance{Engine: f.session.engine, Options: f.session.options}
	if _, err := m.Upgrade(t.Context(), plan, plan.Plan.Digest(), nil); err == nil {
		t.Fatal("upgrade did not leave a pending repair record")
	}
	path := filepath.Join(f.session.app.base, "installed", "rollback.json")
	before := neverReadyRepairRecord(t, path)
	history, applies := f.charts.History, len(f.charts.Applies)
	for _, args := range [][]string{
		{"rollback", "--release-dir", root, "--yes"},
		{"upgrade", "--version", "v0.2.0", "--release-dir", root, "--no-backup", "--yes"},
	} {
		app, _, diagnostic := f.rollbackApplication(t)
		if code := app.run(t.Context(), args); code != 1 {
			t.Fatal("missing source was not refused", args[0], code, diagnostic)
		}
		assertOtherFlatVersionGuidance(t, diagnostic.String(), root)
		assertRequiredRollbackVersion(t, diagnostic.String())
		after := neverReadyRepairRecord(t, path)
		if !bytes.Equal(before, after) || !reflect.DeepEqual(history, f.charts.History) || len(f.charts.Applies) != applies {
			t.Fatal("missing source changed the recovery record or chart", args[0])
		}
	}
}

func assertOtherFlatVersionGuidance(t *testing.T, diagnostic, root string) {
	t.Helper()
	diagnostic = strings.ReplaceAll(diagnostic, ":\n", ": ")
	for _, text := range []string{releaseverify.Version.Error(),
		"Place the signed release in a searched directory, then retry.", "Required release version: v0.1.0",
		"Searched version directory: " + filepath.Join(root, "v0.1.0"), "Searched flat directory: " + root} {
		if !strings.Contains(diagnostic, text) {
			t.Fatal("other signed flat version hid whole lookup guidance", text, diagnostic)
		}
	}
	if strings.Contains(diagnostic, releaseverify.Version.Remedy()) {
		t.Fatal("lookup guidance suggests changing the recorded version", diagnostic)
	}
}
