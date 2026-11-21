package main

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestEveryHelpFitsEightyColumns(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	cases := [][]string{{"help"}}
	for _, row := range commands() {
		cases = append(cases, append([]string{"help"}, strings.Fields(row.path)...))
	}
	for _, flow := range flowCommands() {
		cases = append(cases, append([]string{"help"}, strings.Fields(flow.name)...))
	}
	for _, args := range cases {
		out.Reset()
		diagnostic.Reset()
		if code := app.run(t.Context(), args); code != 0 {
			t.Fatalf("help failed %v: %s", args, diagnostic.String())
		}
		for _, line := range strings.Split(out.String(), "\n") {
			if len([]rune(line)) > 80 && !strings.HasPrefix(line, "oberth ") {
				t.Fatalf("%v: %d columns: %s", args, len([]rune(line)), line)
			}
		}
		if strings.Contains(out.String(), "tool <name> -") {
			t.Fatal("placeholder in help")
		}
		for _, row := range commands() {
			if strings.Contains(row.definition.Name, "_") && strings.Contains(out.String(), row.definition.Name) {
				t.Fatalf("tool name in CLI help: %s", row.definition.Name)
			}
		}
	}
}

func TestCanonicalPositionsAndTypedUsage(t *testing.T) {
	row, _, _ := findCommand(commands(), []string{"issue", "create"})
	raw, problems := toolArgs(row, []string{"Login fails", "Steps: open /login"}, strings.NewReader(""), nil)
	var args apicontract.IssueCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil || len(problems) != 0 || args.Title != "Login fails" || args.Body == nil || *args.Body != "Steps: open /login" {
		t.Fatalf("swapped issue arguments: %s %v", raw, problems)
	}
}

func TestTypedUsageHints(t *testing.T) {
	for _, tc := range []struct {
		words []string
		want  string
	}{
		{[]string{"run", "get", "id", "--step-limit", "wrong"}, "expected integer"},
		{[]string{"run", "get", "id", "--step-limit", "1", "--step-limit", "2"}, "duplicate"},
		{[]string{"run", "list", "--held", "false"}, "--held=false"},
		{[]string{"repo", "list", "--org", "acme"}, "unknown flag --org"},
		{[]string{"run", "list", "-l", "5"}, "unknown flag -l"},
	} {
		app, _, diag := fixture(t, fake.Bindings())
		if code := app.run(t.Context(), tc.words); code != 2 || !strings.Contains(diag.String(), tc.want) {
			t.Fatalf("usage: %v %d %s", tc.words, code, diag.String())
		}
		if strings.Contains(diag.String(), "---l") || strings.Contains(diag.String(), "<command>") {
			t.Fatal("unusable usage hint")
		}
		if strings.Contains(diag.String(), "unexpected positional") && tc.want == "unknown flag --org" {
			t.Fatal("two errors for unknown option")
		}
	}
}

func TestRawInlineJSONAndBooleanFlag(t *testing.T) {
	app, _, diag := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"tool", "repo_list", `{}`}); code != 0 {
		t.Fatalf("inline JSON unreachable: %d %s", code, diag.String())
	}
	row, _, _ := findCommand(commands(), []string{"tool", "run_logs"})
	row.raw = false
	data, problems := toolArgs(row, []string{"id", "--tail"}, nil, nil)
	var args apicontract.RunLogsArgs
	if err := json.Unmarshal(data, &args); err != nil || len(problems) != 0 || args.Tail == nil || !*args.Tail {
		t.Fatal("bare boolean flag lost")
	}
}

func TestToolListHasExactNames(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"tool", "list"}) != 0 {
		t.Fatal("tool list failed")
	}
	names := strings.Fields(out.String())
	if len(names) != len(commands()) {
		t.Fatal("tool list is not the inventory")
	}
	for i, row := range commands() {
		if names[i] != row.definition.Name {
			t.Fatal("tool list does not name tools")
		}
	}
}

func TestVisibleFailureAndCancellationExit(t *testing.T) {
	for _, tc := range []struct {
		cause  error
		code   int
		reason string
	}{
		{publicerr.New(publicerr.Forbidden, "Specific refusal.", "Read the selected operation.", nil), 1, "Specific refusal."},
		{context.Canceled, 130, "interrupted"},
		{context.DeadlineExceeded, 1, "timed out"},
		{&net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}, 1, "Cannot reach"},
	} {
		app, out, diag := fixture(t, fake.Bindings())
		app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) { return nil, tc.cause })
		if code := app.run(t.Context(), []string{"repo", "list"}); code != tc.code || !strings.Contains(diag.String(), tc.reason) || out.Len() != 0 {
			t.Fatalf("wrong visible failure: %d %s", code, diag.String())
		}
	}
}

func TestWaitAnnouncesBeforeBlocking(t *testing.T) {
	app, _, diag := fixture(t, fake.Bindings())
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		if !strings.Contains(diag.String(), "Waiting up to 600s for run abcdefgh to reach finished.") {
			t.Error("silent waiting command")
		}
		return nil, context.Canceled
	})
	if code := app.run(t.Context(), []string{"run", "wait", "abcdefgh"}); code != 130 {
		t.Fatalf("interrupt exit %d", code)
	}
}

func TestCredentialSavedBeforeDisplay(t *testing.T) {
	app, out, diagnostic := fixture(t, credentialBinding("uplink_add", ""))
	path := filepath.Join(filepath.Dir(app.env("OBERTH_TOKEN_FILE")), "new-bearer")
	if code := app.run(t.Context(), []string{"uplink", "add", "--identity", "agent", "--ssh-public-key", "public", "--bearer-file", path, "--json"}); code != 0 {
		t.Fatalf("bearer activation: %d %s", code, diagnostic.String())
	}
	assertSavedCredential(t, out.Bytes(), "")
}

func TestCredentialDestinationRefusesBeforeMutation(t *testing.T) {
	b := fake.Bindings()
	app, _, diag := fixture(t, b)
	app.base = t.TempDir()
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	// A public credential directory itself must fail before calling the service.
	dir := app.base + "/credentials"
	if err := os.Mkdir(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0777); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"uplink", "add", "--identity", "agent", "--ssh-public-key", "public", "--bearer-file", filepath.Join(dir, "new-bearer")}) != 1 {
		t.Fatalf("unsafe destination accepted: %s", diag.String())
	}
	_, _, calls := b.UplinkAdd.Handler.(*fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]).Observed()
	if calls != 0 || !strings.Contains(diag.String(), dir) {
		t.Fatal("mutated before reserving credential")
	}
}

func TestCompactRunsEmptyAndMissingValues(t *testing.T) {
	app, out, _ := fixture(t, fake.Bindings())
	data := []byte(`{"runs":[{"id":"1234567890abcdef","status":"failed","ref":"refs/heads/main","display_reason":"image pull failed","steps":[{"name":"test","status":"failed","display_reason":"image pull failed"}]}]}`)
	if err := app.render(data, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	if len(strings.Split(strings.TrimSpace(out.String()), "\n")) != 2 || strings.Contains(out.String(), "steps[") || !strings.Contains(out.String(), "image pull failed") {
		t.Fatal("run list is not compact")
	}
	out.Reset()
	data = []byte(`{"id":"12345678","status":"failed","ref":"main","finished_at":null,"steps":[{"name":"build","status":"failed","display_reason":"image pull failed","detail":"native detail"}]}`)
	if err := app.render(data, false, commonOptions{}); err != nil || !strings.Contains(out.String(), "build  failed  image pull failed") || strings.Contains(out.String(), "native detail") {
		t.Fatalf("run view missing: %s %v", out.String(), err)
	}
	out.Reset()
	if err := app.render([]byte(`{"issues":[],"latest_run":null,"reason":""}`), false, commonOptions{}); err != nil || out.String() != "no issues\n" {
		t.Fatalf("empty output: %s %v", out.String(), err)
	}
}

func TestLongFactsAndRefusalShape(t *testing.T) {
	app, out, diag := fixture(t, fake.Bindings())
	text := strings.Repeat("word ", 50)
	raw, _ := json.Marshal(map[string]string{"public": text})
	if err := app.render(raw, false, commonOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if len(line) > 80 {
			t.Fatal("fact line exceeds 80 columns")
		}
	}
	for _, raw := range []string{`{"reason":"Refused."}`, `{"remedy":"Read state."}`} {
		if err := app.render([]byte(raw), true, commonOptions{}); err == nil {
			t.Fatal("incomplete refusal accepted")
		}
	}
	raw = []byte(`{"reason":"Refused.","remedy":"Read state.","detail":"visible detail"}`)
	diag.Reset()
	if err := app.render(raw, true, commonOptions{}); err != nil || strings.Contains(diag.String(), "visible detail") {
		t.Fatal("refusal detail leaked")
	}
	if err := app.render(raw, true, commonOptions{detail: true}); err != nil || !strings.Contains(diag.String(), "visible detail") {
		t.Fatal("refusal detail missing")
	}
}

func TestMissingFileShowsPathAndNoPrivateCause(t *testing.T) {
	app, out, diag := fixture(t, fake.Bindings())
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if app.run(t.Context(), []string{"repo", "list", "--token-file", dir + "/missing"}) != 1 || !strings.Contains(diag.String(), "not found") || !strings.Contains(diag.String(), "/missing") || out.Len() != 0 {
		t.Fatalf("missing file lacks its path: %s", diag.String())
	}
	cause := errors.New("private-cause")
	if code := app.failed(publicerr.New(publicerr.Unavailable, "Public reason.", "Read the result.", cause)); code != 1 || strings.Contains(diag.String(), cause.Error()) {
		t.Fatal("private cause leaked")
	}
}

func credentialBinding(name, _ string) apicontract.Bindings {
	b := fake.Bindings()
	switch name {
	case "uplink_add":
		b.UplinkAdd.Handler = &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	case "uplink_cert":
		b.UplinkCert.Handler = &fake.Handler[apicontract.UplinkCertArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	case "uplink_rotate":
		b.UplinkRotate.Handler = &fake.Handler[apicontract.UplinkRotateArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	case "client_renew":
		b.ClientRenew.Handler = &fake.Handler[apicontract.ClientRenewArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	}
	return b
}
func assertSavedCredential(t *testing.T, raw []byte, secret string) {
	t.Helper()
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := json.Unmarshal(result["token_file"], &path); err != nil {
		t.Fatal("private path absent")
	}
	data, err := client.ReadPrivate(path)
	if err != nil || len(data) == 0 || secret != "" && string(data) != secret {
		t.Fatal("credential not retained")
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("credential not private")
	}
}

func TestRefusedArgumentNamesField(t *testing.T) {
	app, _, diag := fixture(t, fake.Bindings())
	if code := app.run(t.Context(), []string{"issue", "get", "0"}); code != 1 || !strings.Contains(diag.String(), "Invalid request argument: id.") {
		t.Fatalf("field hidden: %d %s", code, diag.String())
	}
}

func TestUnknownCommandOffersNearMatch(t *testing.T) {
	app, _, diag := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"run", "lsit"}) != 2 || !strings.Contains(diag.String(), "run lsit") || !strings.Contains(diag.String(), "Did you mean oberth run list?") {
		t.Fatal("typo hint lacks a close command")
	}
}

func TestNilPageListSaysEmpty(t *testing.T) {
	app, out, diag := fixture(t, fake.Bindings())
	if app.run(t.Context(), []string{"issue", "list"}) != 0 || out.String() != "no issues\n" {
		t.Fatalf("empty list disappeared: %s %s", out.String(), diag.String())
	}
}

func TestNamedBearerCreationRetainsCredential(t *testing.T) {
	b := credentialBinding("uplink_add", "")
	app, out, diagnostic := fixture(t, b)
	path := filepath.Join(filepath.Dir(app.env("OBERTH_TOKEN_FILE")), "new-bearer")
	h := b.UplinkAdd.Handler.(*fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity])
	h.Run = func(context.Context) (apicontract.Identity, error) {
		if saved, err := client.ReadPrivate(path); err != nil || len(saved) == 0 {
			return apicontract.Identity{}, errors.New("activation preceded private persistence")
		}
		return apicontract.Identity{Identity: "agent"}, nil
	}
	if code := app.run(t.Context(), []string{"uplink", "add", "--identity", "agent", "--ssh-public-key", "public", "--bearer-file", path, "--json"}); code != 0 {
		t.Fatalf("activation failed: %d %s", code, diagnostic.String())
	}
	input, _, calls := h.Observed()
	secret, err := client.ReadPrivate(path)
	if err != nil || calls != 1 || input[0].BearerDigest != apicontract.SecretDigest(string(secret)) || strings.Contains(out.String(), string(secret)) {
		t.Fatal("only the retained bearer digest must be sent once", err)
	}
	assertSavedCredential(t, out.Bytes(), string(secret))
}

func TestBearerPathCanBeCopiedFromHumanOutput(t *testing.T) {
	app, out, diagnostic := fixture(t, credentialBinding("uplink_add", ""))
	path := filepath.Join(filepath.Dir(app.env("OBERTH_TOKEN_FILE")), "new-bearer")
	if app.run(t.Context(), []string{"uplink", "add", "--identity", "agent", "--ssh-public-key", "public", "--bearer-file", path}) != 0 {
		t.Fatal(diagnostic.String())
	}
	secret, err := client.ReadPrivate(path)
	if err != nil || !strings.Contains(out.String(), path) || strings.Contains(out.String(), string(secret)) {
		t.Fatal("saved path is split or credential displayed", err)
	}
}
