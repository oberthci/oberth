package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestTopHelpIncludesEveryCatalogueSummary(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"--help"}) != 0 {
		t.Fatal(diagnostic.String())
	}
	text := strings.Join(strings.Fields(out.String()), " ")
	for name, row := range commandCatalogue() {
		if row.Summary == "" || !strings.Contains(text, name+": "+row.Summary) {
			t.Errorf("missing catalogue summary: %s", name)
		}
	}
	for _, group := range []string{"Getting started:", "Daily use:", "Administration:"} {
		if strings.Count(out.String(), group) != 1 {
			t.Errorf("missing or repeated group: %s", group)
		}
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len([]rune(line)) > 80 {
			t.Fatal("help exceeds 80 columns")
		}
	}
}

func TestRestoreHasNoCommandOrInstallerPath(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
		t.Fatal("restore reached installation owner")
		return nil
	}
	for _, args := range [][]string{{"restore", "archive", "--yes"}, {"restore", "archive", "--dry-run"}, {"help", "restore"}} {
		diagnostic.Reset()
		if code := app.run(t.Context(), args); code != 2 || !strings.Contains(diagnostic.String(), "Unknown command") {
			t.Fatal("restore is a command", code, diagnostic.String())
		}
	}
	if _, found := commandCatalogue()["restore"]; found {
		t.Fatal("restore catalogued")
	}
	session := installationSession{request: installationRequest{name: "restore", flags: installationFlags("restore")}}
	err := session.dispatch(t.Context(), nil, nil)
	var usage usageProblems
	if !errors.As(err, &usage) {
		t.Fatal("unknown installer action entered an effect path", err)
	}
}

func TestHumanActionsHaveOneNameAndKeepRawRoutes(t *testing.T) {
	rows := commandCatalogue()
	for _, name := range []string{"logs", "run logs", "run check", "audit list", "uplink invite", "client renew", "run hold abandon", "status", "wait", "sync status"} {
		if _, ok := rows[name]; ok {
			t.Errorf("duplicate or ambiguous human action: %s", name)
		}
	}
	for _, name := range []string{"log", "check", "audit", "invite", "renew", "run hold release", "commit status", "commit wait", "run wait", "system status", "lock status"} {
		if _, ok := rows[name]; !ok {
			t.Errorf("missing kept name: %s", name)
		}
	}
	for _, name := range []string{"logs", "run_logs", "run_check", "audit_list", "uplink_invite", "client_renew", "run_hold_abandon", "status", "wait", "sync_status"} {
		row, _, found := findCommand(commands(), []string{"tool", name})
		if !found || !row.raw || row.definition.Name != name {
			t.Errorf("raw route lost: %s", name)
		}
	}
}

func TestValidateFileAndSuccessOutput(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.directory = t.TempDir()
	for path, reason := range map[string]string{".oberth/build.yaml": "not found", "../outside.yaml": "outside the checkout"} {
		diagnostic.Reset()
		if app.run(t.Context(), []string{"validate", path}) != 1 {
			t.Fatal("unusable file accepted")
		}
		if !strings.Contains(diagnostic.String(), path) || !strings.Contains(diagnostic.String(), reason) || strings.Contains(diagnostic.String(), "listed fields") {
			t.Fatal("unhelpful file refusal", diagnostic.String())
		}
	}
	if err := os.WriteFile(filepath.Join(app.directory, "pipeline.yaml"), []byte(starterWorkflow()), 0600); err != nil {
		t.Fatal(err)
	}
	diagnostic.Reset()
	if app.run(t.Context(), []string{"validate", "pipeline.yaml"}) != 0 {
		t.Fatal(diagnostic.String())
	}
	if !strings.HasPrefix(out.String(), "build\n") || strings.Contains(out.String(), "entries[") || strings.Contains(out.String(), "credential intent") {
		t.Fatal("generic facts instead of step names", out.String())
	}
}

func TestRefusalRenderingIsSharedAndComplete(t *testing.T) {
	refusal := publicerr.New(publicerr.Forbidden, "Access refused.", "Ask an administrator for access.", errors.New("private-cause"))
	refusal.Context = []string{"Required context."}
	refusal.Detail = "First detail.\nSecond detail."
	for _, detail := range []bool{false, true} {
		app, _, diagnostic := fixture(t, fake.Bindings())
		app.failedDetail(refusal, detail)
		want := diagnostic.String()
		diagnostic.Reset()
		session := flowSession{app: app, options: commonOptions{detail: detail}}
		session.failed(refusal)
		if diagnostic.String() != want {
			t.Fatal("flow renderer differs", diagnostic.String(), want)
		}
		diagnostic.Reset()
		raw, err := refusal.MarshalJSON()
		if err != nil {
			t.Fatal(err)
		}
		if err = app.renderRefusal(raw, commonOptions{detail: detail}); err != nil {
			t.Fatal(err)
		}
		if diagnostic.String() != want || strings.Count(want, refusal.Reason()) != 1 || strings.Contains(want, "private-cause") {
			t.Fatal("duplicate or private refusal", want)
		}
		assertRefusalDetail(t, want, refusal.Detail, detail)
	}
}

func TestInstallFlagsUseReviewNamesWithoutAliases(t *testing.T) {
	for _, old := range []string{"--no-pod-floor", "--no-egress"} {
		app, _, _ := fixture(t, fake.Bindings())
		app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error {
			t.Fatal("retired flag reached owner")
			return nil
		}
		if app.run(t.Context(), []string{"install", old}) != 2 {
			t.Fatal("retired flag accepted", old)
		}
	}
	flags := installationFlags("install")
	if flags.Lookup("no-pod-defaults") == nil || flags.Lookup("no-link-local-filter") == nil {
		t.Fatal("review names absent")
	}
	var output bytes.Buffer
	if err := writeWrapped(&output, commandCatalogue()["backup"].Summary); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "safekeeping") || !strings.Contains(output.String(), "not in this release") {
		t.Fatal("backup purpose missing")
	}
}

func assertRefusalDetail(t *testing.T, text, detailText string, detail bool) {
	t.Helper()
	if detail {
		if !strings.Contains(text, detailText) {
			t.Fatal("detail cut", text)
		}
		return
	}
	if !strings.Contains(text, "--detail") || strings.Contains(text, "First detail") {
		t.Fatal("detail silently omitted", text)
	}
}
