package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestEveryCatalogueCommandHasMatchingHumanHelp(t *testing.T) {
	for name, syntax := range commandCatalogue() {
		t.Run(name, func(t *testing.T) { assertHumanHelp(t, name, syntax) })
	}
}
func assertHumanHelp(t *testing.T, name string, syntax commandSyntax) {
	t.Helper()
	app, out, diagnostic := fixture(t, fake.Bindings())
	words := strings.Fields(name)
	if code := app.run(t.Context(), append([]string{"help"}, words...)); code != 0 {
		t.Fatalf("help failed: %s", diagnostic.String())
	}
	first := out.String()
	out.Reset()
	if code := app.run(t.Context(), append(words, "--help")); code != 0 || out.String() != first {
		t.Fatalf("help forms differ: %s %s", first, out.String())
	}
	if !strings.HasPrefix(first, "oberth "+name) {
		t.Fatal("missing command synopsis")
	}
	flags := map[string]int{}
	pattern := regexp.MustCompile(`--[a-z][a-z0-9-]*|(?:^| )-[oh](?: |:|$)`)
	for _, line := range strings.Split(first, "\n") {
		if !strings.HasPrefix(line, "-") {
			continue
		}
		for _, flag := range pattern.FindAllString(line, -1) {
			flag = strings.TrimSpace(strings.TrimSuffix(flag, ":"))
			arity, ok := syntax.Flags[flag]
			if !ok {
				t.Fatalf("help advertises unused flag %s", flag)
			}
			flags[flag] = arity
		}
	}
	if !reflect.DeepEqual(flags, syntax.Flags) {
		t.Fatalf("help flags %v != parser %v", flags, syntax.Flags)
	}
	assertHelpProseWidth(t, first)
}

func assertHelpProseWidth(t *testing.T, text string) {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if len([]rune(line)) > 80 && !strings.HasPrefix(line, "oberth ") {
			t.Fatal("help exceeds 80 columns")
		}
	}
}
func TestStatusNamesExplainDifferentSubjects(t *testing.T) {
	rows := commandCatalogue()
	for name, summary := range map[string]string{"commit status": "Show a commit", "system status": "Show installation health", "lock status": "Show native lock"} {
		if !strings.HasPrefix(rows[name].Summary, summary) {
			t.Fatal("ambiguous status", name, rows[name])
		}
	}
}

func TestMCPDiagnosticsNeverEnterProtocolOutput(t *testing.T) {
	app, diagnostic := pinnedMCPApplication(t)
	var out bytes.Buffer
	app.out = &out
	frame, _ := json.Marshal(modernMCPFrame(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "run_get", "arguments": map[string]string{"id": "12345678"}}}))
	app.in = strings.NewReader(string(frame) + "\n")
	now := app.config.Time()
	app.config.Now = func() time.Time { return now }
	app.config.Wait = func(_ context.Context, delay time.Duration) error { now = now.Add(delay); return nil }
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) { return nil, io.EOF })
	if code := app.run(t.Context(), []string{"mcp", "--profile", "pinned"}); code != 0 {
		t.Fatalf("bridge failed: %s", diagnostic.String())
	}
	message := "No reply yet, trying again."
	if !strings.Contains(diagnostic.String(), message) || bytes.Contains(out.Bytes(), []byte(message)) || !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatalf("diagnostic reached protocol output: %q / %q", out.String(), diagnostic.String())
	}
}
func TestFlowWaitLinesNameTheirSubjects(t *testing.T) {
	rows := commands()
	cases := map[string]any{"wait": apicontract.WaitArgs{Repo: "project", SHA: "abc123"}, "events_wait": apicontract.EventsWaitArgs{Kinds: []string{"release.published"}, RunIDs: []string{"run1"}}, "promote_status": apicontract.PromoteStatusArgs{ID: "promotion1"}}
	for name, args := range cases {
		for _, row := range rows {
			if row.definition.Name != name {
				continue
			}
			line := waitingForArgs(row.definition, args)
			if strings.Contains(line, "for wait.") || strings.Contains(line, "for events wait") || strings.Contains(line, "for promote status") {
				t.Fatal("waiting names tool", line)
			}
		}
	}
}
func TestCatalogueIncludesRulingPaths(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	if app.run(context.Background(), []string{"help", "-o", "json"}) != 0 {
		t.Fatal("catalogue failed")
	}
	var wire struct{ Commands map[string]commandSyntax }
	if json.Unmarshal(out.Bytes(), &wire) != nil {
		t.Fatal("invalid catalogue")
	}
	for _, name := range []string{"secretstore exec", "renew"} {
		if _, ok := wire.Commands[name]; !ok {
			t.Fatal("ruling entry missing", name)
		}
	}
}

func TestStatusPositionalRefUsesCICommand(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.StatusArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunPassed}}
	b.Status.Handler = h
	app, _, diagnostic := fixture(t, b)
	if app.run(t.Context(), []string{"commit", "status", "abc123"}) != 0 {
		t.Fatal("status ref refused", diagnostic.String())
	}
	inputs, _, calls := h.Observed()
	if calls != 1 || inputs[0].Ref != "abc123" {
		t.Fatal("status positional did not reach ref")
	}
}

func TestHelpAfterArgumentsUsesTheRunningCommand(t *testing.T) {
	cases := map[string][]string{"promote": {"sha", "--repo", "project"}, "repo add": {"https://forge.example/team/project", "--known-hosts", "file"}, "install": {"--context", "chosen"}, "version": {"--schema"}, "mcp": {"--profile", "uplink"}}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			app, out, diag := fixture(t, fake.Bindings())
			words := strings.Fields(name)
			if app.run(t.Context(), append([]string{"help"}, words...)) != 0 {
				t.Fatal(diag.String())
			}
			want := out.String()
			out.Reset()
			request := append(append(words, args...), "--help")
			if app.run(t.Context(), request) != 0 || out.String() != want {
				t.Fatalf("argument help changed declaration: %s / %s / %s", want, out.String(), diag.String())
			}
		})
	}
}
