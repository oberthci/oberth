package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"github.com/oberthci/oberth/internal/store"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestRunAndStepTablesAlignColumns(t *testing.T) {
	for _, data := range []string{
		`{"runs":[{"id":"12345678","status":"failed","ref":"refs/heads/main","display_reason":"first reason"},{"id":"87654321","status":"passed","ref":"refs/heads/very-long-branch","display_reason":"second reason"}]}`,
		`{"id":"12345678","status":"failed","ref":"main","steps":[{"name":"x","status":"failed","display_reason":"first reason"},{"name":"long-step-name","status":"passed","display_reason":"second reason"}]}`,
	} {
		app, out, _ := fixture(t, fake.Bindings())
		if err := app.render([]byte(data), false, commonOptions{}); err != nil {
			t.Fatal(err)
		}
		positions := []int{}
		for _, line := range strings.Split(out.String(), "\n") {
			if strings.TrimRight(line, " ") != line {
				t.Fatalf("trailing table space %q", line)
			}
			for _, reason := range []string{"REASON", "first reason", "second reason"} {
				if n := strings.Index(line, reason); n >= 0 {
					positions = append(positions, n)
				}
			}
		}
		if len(positions) != 3 || positions[0] != positions[1] || positions[1] != positions[2] {
			t.Fatalf("unaligned table: %s", out.String())
		}
	}
}

func TestRunLabelsAndRepositoryLatestRunStayReadable(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	data := []byte(`{"id":"12345678","status":"failed","ref":"main","commit_sha":"abc","display_reason":"image pull failed","next_step_offset":2,"failure":{"retry_hint":{"action":"none"}}}`)
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"commit: abc", "reason: image pull failed", "next step offset: 2", "retry hint / action: none"} {
		if !strings.Contains(out.String(), label) {
			t.Fatalf("missing label %q: %s", label, out.String())
		}
	}
	if strings.Contains(out.String(), "_") {
		t.Fatal("raw run keys displayed")
	}
	out.Reset()
	data = []byte(`{"repositories":[{"repo":"acme/app","latest_run":{"id":"12345678","status":"failed","ref":"main","display_reason":"image pull failed"}},{"repo":"acme/other"}]}`)
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "latest run: 12345678  failed  image pull failed") || strings.Contains(out.String(), "Run 12345678") {
		t.Fatalf("unlabelled nested run: %s", out.String())
	}
}

// TestRepositoryRecentRunsReadAsOneLine: oberth repo list --recent-runs prints a
// repository's strip as one line of letters, oldest first, not as run views, and
// "no runs" for the empty list the server sends for a repository without runs.
// Without the flag the server sends no list and no line is printed.
func TestRepositoryRecentRunsReadAsOneLine(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	at := time.Date(2026, 11, 16, 14, 0, 0, 0, time.UTC)
	var marks []stateport.RunMark
	for i, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled, stepmodel.RunQueued, stepmodel.RunRunning} {
		marks = append(marks, stateport.RunMark{ID: strconv.Itoa(11111111 * (i + 1)), Status: status, Ref: "refs/heads/main", CreatedAt: at})
	}
	marks = append(marks, stateport.RunMark{ID: "66666666", Status: stepmodel.RunPassed, Ref: "refs/heads/main", CreatedAt: at, DisplayReason: "assisted; not evidence"})
	data, err := json.Marshal(apicontract.RepoPage{Repositories: []apicontract.Repository{
		{Repo: "acme/app", RecentRuns: marks}, {Repo: "acme/quiet", RecentRuns: []stateport.RunMark{}}, {Repo: "acme/plain"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if !strings.Contains(text, "repositories[0] / recent runs: PF-RR- (oldest first)\n") || !strings.Contains(text, "repositories[1] / recent runs: no runs\n") ||
		strings.Contains(text, "repositories[2] / recent runs") || strings.Contains(text, "11111111") || strings.Contains(text, "refs/heads") {
		t.Fatalf("recent runs not one line per repository: %q", text)
	}
}

func TestWrappedHelpContinuationsAreIndented(t *testing.T) {
	var out bytes.Buffer
	text := "--long-option: " + strings.Repeat("complete words ", 30)
	if err := writeWrapped(&out, text); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if len(lines) < 3 {
		t.Fatal("help did not wrap")
	}
	for i, line := range lines {
		if len([]rune(line)) > 80 || i > 0 && !strings.HasPrefix(line, "  ") {
			t.Fatalf("unreadable continuation %q", line)
		}
	}
	if strings.Join(strings.Fields(out.String()), " ") != strings.Join(strings.Fields(text), " ") {
		t.Fatal("help words were lost")
	}
}

func TestCredentialReservationsRemovedAfterRemoteFailure(t *testing.T) {
	for _, kind := range []string{"refusal", "error", "empty"} {
		t.Run(kind, func(t *testing.T) {
			b := fake.Bindings()
			h := &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{}
			if kind == "refusal" {
				h.Error = publicerr.New(publicerr.Forbidden, "Caller refused.", "Ask the administrator.", nil)
			}
			b.UplinkAdd.Handler = h
			app, _, _ := fixture(t, b)
			base := filepath.Dir(app.env("OBERTH_TOKEN_FILE"))
			if kind == "error" {
				app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) { return nil, io.EOF })
			}
			input, _ := json.Marshal(sample(b.UplinkAdd.Definition(apicontract.Metadata{}).InputType))
			code := app.run(t.Context(), []string{"tool", "uplink_add", string(input)})
			if kind == "empty" && code != 0 || kind != "empty" && code != 1 {
				t.Fatalf("wrong exit %d", code)
			}
			entries, err := os.ReadDir(filepath.Join(base, "credentials"))
			if err != nil && !os.IsNotExist(err) || len(entries) != 0 {
				t.Fatalf("unused private files left after %s: %v %d", kind, err, len(entries))
			}
		})
	}
}

func TestCommandCatalogueUsesParserFlagsAndBounds(t *testing.T) {
	rows := commandCatalogue()
	if _, ok := rows["runs"]; ok {
		t.Fatal("retired alias in real table")
	}
	if rows["run get"].Args != [2]int{1, 1} {
		t.Fatal("wrong run get bounds")
	}
	if _, ok := rows["run wait"].Flags["--timeout-s"]; !ok {
		t.Fatal("canonical wait flag missing")
	}
	if _, ok := rows["invite"].Flags["--admin"]; ok {
		t.Fatal("unsupported invitation flag advertised")
	}
	for _, flow := range flowCommands() {
		syntax := rows[flow.name]
		if syntax.Args != [2]int{flow.minArgs, flow.maxArgs} {
			t.Fatal("flow bounds drift")
		}
	}
	app, out, _ := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"help", "-o", "json"}); code != 0 || !json.Valid(out.Bytes()) {
		t.Fatal("real command table unreadable")
	}
}

func TestInstallerSyntaxRefusesBeforeCallingOwner(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	called := 0
	app.install = func(context.Context, installationRequest, io.Reader, io.Writer) error { called++; return nil }
	if code := app.run(t.Context(), []string{"install", "--imaginary"}); code != 2 || called != 0 {
		t.Fatal("invalid flags reached owner")
	}
	app.install = nil
	diagnostic.Reset()
	if code := app.run(t.Context(), []string{"install", "--dry-run"}); code != 1 || !strings.Contains(diagnostic.String(), "Select an explicit kubeconfig context") {
		t.Fatalf("default engine did not require an explicit target: %d %s", code, diagnostic.String())
	}
	out.Reset()
	if code := app.run(t.Context(), []string{"version", "--schema"}); code != 0 || strings.TrimSpace(out.String()) != strconv.Itoa(store.Schema) {
		t.Fatal("canonical schema version missing")
	}
}

func TestInstallerOwnerReceivesParsedOverrides(t *testing.T) {
	app, _, _ := fixture(t, fake.Bindings())
	called := false
	app.install = func(_ context.Context, request installationRequest, _ io.Reader, _ io.Writer) error {
		called = true
		if request.name != "upgrade" || len(request.args) != 0 {
			t.Fatal("wrong owner request")
		}
		if request.flags.Lookup("version").Value.String() != "v1.2.3" {
			t.Fatal("parsed version lost")
		}
		supplied := map[string]bool{}
		request.flags.Visit(func(value *flag.Flag) { supplied[value.Name] = true })
		if !supplied["version"] || !supplied["dry-run"] || supplied["no-backup"] {
			t.Fatal("explicit overrides confused with defaults")
		}
		return context.Canceled
	}
	if code := app.run(t.Context(), []string{"upgrade", "--version", "v1.2.3", "--dry-run"}); code != 130 || !called {
		t.Fatal("parsed request did not reach owner")
	}
}
