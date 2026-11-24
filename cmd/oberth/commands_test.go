package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/publicerr"
)

type testTransport func(*http.Request) (*http.Response, error)

func (f testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type response struct{ *httptest.ResponseRecorder }

func (response) SetWriteDeadline(time.Time) error { return nil }

func fixture(t *testing.T, bindings apicontract.Bindings) (application, *bytes.Buffer, *bytes.Buffer) {
	t.Helper()
	clock := fake.NewClock()
	registry, err := api.Builtin(bindings)
	if err != nil {
		t.Fatal(err)
	}
	auth := fake.NewAuth(apicontract.Caller{Actor: actor.Actor{Identity: "operator", AuthMethod: "bearer", Admin: true}, CredentialID: "credential"})
	s, err := api.New(api.Config{Registry: registry, Auth: auth, Classify: publicerr.Classify, Runtime: clock.Runtime(), FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.BeginDrain)
	dir := t.TempDir()
	if err = os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "token")
	if err = client.WritePrivate(path, []byte("valid")); err != nil {
		t.Fatal(err)
	}
	out, diagnostic := &bytes.Buffer{}, &bytes.Buffer{}
	app := application{in: strings.NewReader(""), out: out, diagnostic: diagnostic, env: func(name string) string {
		if name == "OBERTH_URL" {
			return "https://127.0.0.1"
		}
		if name == "OBERTH_TOKEN_FILE" {
			return path
		}
		return ""
	}, config: client.Config{Now: clock.Now, Wait: func(context.Context, time.Duration) error { t.Fatal("unexpected retry"); return nil }, Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		w := response{httptest.NewRecorder()}
		s.Handler().ServeHTTP(w, r)
		return w.Result(), nil
	})}}
	return app, out, diagnostic
}

func sample(t reflect.Type) any {
	if t.Kind() == reflect.Pointer {
		return sample(t.Elem())
	}
	switch t.Kind() {
	case reflect.Struct:
		object := map[string]any{}
		for _, f := range argumentFields(t) {
			if f.required {
				object[f.name] = sample(f.field.Type)
			}
		}
		if t == reflect.TypeFor[apicontract.UplinkAddArgs]() || t == reflect.TypeFor[apicontract.UplinkInviteArgs]() {
			object["scope"] = []string{"*"}
		}
		return object
	case reflect.String:
		return "project"
	case reflect.Int, reflect.Int64:
		return 1
	case reflect.Bool:
		return true
	case reflect.Slice:
		return []any{sample(t.Elem())}
	case reflect.Map:
		return map[string]string{"item": "value"}
	}
	return nil
}

func observedCalls(v reflect.Value) int {
	total := 0
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Anonymous {
			total += observedCalls(v.Field(i))
			continue
		}
		h := v.Field(i).FieldByName("Handler").Elem()
		values := h.MethodByName("Observed").Call(nil)
		total += int(values[2].Int())
	}
	return total
}

func TestEveryToolReachable(t *testing.T) {
	rows := commands()
	definitions := apicontract.Definitions(apicontract.Bindings{})
	if len(rows) != len(definitions) {
		t.Fatal("tool/command inventory differs")
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if seen[row.definition.Name] {
			t.Fatal("duplicate row")
		}
		seen[row.definition.Name] = true
		t.Run(row.definition.Name, func(t *testing.T) { assertToolReachable(t, row) })
	}
	for _, d := range definitions {
		if !seen[d.Name] {
			t.Fatal("missing tool", d.Name)
		}
	}
	assertHelpInventory(t)
}

func TestToolArgsUsage(t *testing.T) {
	app, _, diagnostic := fixture(t, fake.Bindings())
	code := app.run(context.Background(), []string{"upstream", "add", "--unknown-a", "--unknown-b"})
	if code != 2 {
		t.Fatalf("usage exit %d", code)
	}
	for _, text := range []string{"unknown flag --unknown-a", "unknown flag --unknown-b", "missing --base-url", "missing --name"} {
		if !strings.Contains(diagnostic.String(), text) {
			t.Fatal("missing usage problem", text)
		}
	}
	for _, args := range [][]string{{"run", "get", "--step-limit", "wrong"}, {"run", "get", "--id"}, {"run", "get", "id", "extra"}, {"tool", "missing"}, {"serve"}, {"repo", "list", "-o", "yaml"}} {
		if app.run(context.Background(), args) != 2 {
			t.Fatalf("accepted invalid usage: %v", args)
		}
	}
}

func TestRemoteTransportFlags(t *testing.T) {
	for _, row := range commands() {
		t.Run(row.path, func(t *testing.T) {
			app, out, diagnostic := fixture(t, fake.Bindings())
			input, _ := json.Marshal(sample(row.definition.InputType))
			app.in = bytes.NewReader(input)
			var got *http.Request
			base := app.config.Transport
			app.config.Transport = testTransport(func(r *http.Request) (*http.Response, error) { got = r; return base.RoundTrip(r) })
			token := app.env("OBERTH_TOKEN_FILE")
			flags := []string{"--url", "https://localhost:8123", "--token-file", token, "--ca-file", "pin", "--client-cert", "cert", "--client-key", "key"}
			args := append(flags, "tool", row.definition.Name, "-")
			if app.run(context.Background(), args) != 0 || got == nil || got.URL.Host != "localhost:8123" || got.Header.Get("Authorization") != "Bearer valid" {
				t.Fatalf("transport override failed: %s", diagnostic.String())
			}
			assertParsedTransport(t, flags)
			out.Reset()
			if app.run(context.Background(), []string{"help", "tool", row.definition.Name}) != 0 {
				t.Fatal("help failed")
			}
			for _, flag := range transportFlags(&commonOptions{}) {
				if !strings.Contains(out.String(), "--"+flag.name+" (") {
					t.Fatal("transport flag absent from help")
				}
			}
		})
	}
}

func TestNoPrivateKeyInArgvOrOutput(t *testing.T) {
	app, out, diagnostic := fixture(t, fake.Bindings())
	for _, args := range [][]string{
		{"upstream", "provide", "key", "--name", "upstream", "--private-key", "sensitive-value"},
		{"tool", "upstream_provide_key", `{"name":"upstream","private_key":"sensitive-value"}`},
	} {
		if app.run(context.Background(), args) != 2 || strings.Contains(out.String()+diagnostic.String(), "sensitive-value") {
			t.Fatal("private argument accepted or echoed")
		}
	}
	b := fake.Bindings()
	b.UplinkAdd.Handler = &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	app, out, diagnostic = fixture(t, b)
	app.in = strings.NewReader(`{"identity":"agent","scope":["*"],"ssh_public_key":"public"}`)
	if app.run(context.Background(), []string{"tool", "uplink_add", "-", "-o", "json"}) != 0 || strings.Contains(out.String()+diagnostic.String(), "sensitive-value") {
		t.Fatal("credential result leaked")
	}
}

func assertToolReachable(t *testing.T, row command) {
	t.Helper()
	if row.definition.Name == "secret_put" {
		assertPrivateSecretCommandReachable(t, row)
		return
	}
	b := fake.Bindings()
	app, out, diagnostic := fixture(t, b)
	input, _ := json.Marshal(sample(row.definition.InputType))
	app.in = bytes.NewReader(input)
	code := app.run(context.Background(), []string{"tool", row.definition.Name, "-", "-o", "json"})
	if code != 0 || !json.Valid(out.Bytes()) || (!row.definition.LongPoll && diagnostic.Len() != 0) || observedCalls(reflect.ValueOf(b)) != 1 {
		t.Fatalf("tool unreachable: exit %d, diagnostic %s", code, diagnostic.String())
	}
	out.Reset()
	args := []string{"help", "tool", row.definition.Name}
	if app.run(context.Background(), args) != 0 || !strings.Contains(out.String(), row.definition.Summary) {
		t.Fatal("command help missing")
	}
}

func assertHelpInventory(t *testing.T) {
	t.Helper()
	app, out, _ := fixture(t, fake.Bindings())
	if app.run(context.Background(), []string{"help"}) != 0 {
		t.Fatal("help failed")
	}
	for name, row := range commandCatalogue() {
		normalized := strings.Join(strings.Fields(out.String()), " ")
		if !strings.Contains(normalized, name+": "+row.Summary) {
			t.Fatal("catalogued command or summary missing", name)
		}
	}
}

func assertParsedTransport(t *testing.T, flags []string) {
	t.Helper()
	resolved, _, problems := parseCommon(flags)
	if len(problems) > 0 || resolved.transport.CAFile != "pin" || resolved.transport.ClientCert != "cert" || resolved.transport.ClientKey != "key" {
		t.Fatal("transport flags lost")
	}
}

func TestPointerAndCompoundFlags(t *testing.T) {
	row, _, ok := findCommand(commands(), []string{"tool", "run_logs"})
	if !ok {
		t.Fatal("missing row")
	}
	row.raw = false
	data, problems := toolArgs(row, []string{"run-id", "--pattern", "error.*", "--tail=false", "--limit", "12"}, strings.NewReader(""), nil)
	var args apicontract.RunLogsArgs
	if len(problems) != 0 || json.Unmarshal(data, &args) != nil {
		t.Fatalf("pointer flags: %v", problems)
	}
	if args.Pattern == nil || *args.Pattern != "error.*" || args.Tail == nil || *args.Tail || args.Limit == nil || *args.Limit != 12 {
		t.Fatal("pointer arguments changed")
	}
}

func TestCompoundFlags(t *testing.T) {
	row, _, ok := findCommand(commands(), []string{"run", "hold", "exec"})
	if !ok {
		t.Fatal("missing row")
	}
	_, problems := toolArgs(row, []string{"--id", "run-id", "--step", "build", "--argv", `["pwd"]`}, strings.NewReader(""), nil)
	if len(problems) != 0 {
		t.Fatalf("compound flags: %v", problems)
	}
}

func assertPrivateSecretCommandReachable(t *testing.T, row command) {
	t.Helper()
	bindings := fake.Bindings()
	handler := bindings.SecretPut.Handler.(*fake.Handler[apicontract.SecretPutArgs, apicontract.Receipt])
	handler.Run = func(ctx context.Context) (apicontract.Receipt, error) {
		value, ok := apicontract.SecretValue(ctx)
		if !ok || string(value) != "private-fixture" {
			t.Fatal("secret command did not use the private body")
		}
		return apicontract.Receipt{ID: "registry/key", State: "stored"}, nil
	}
	app, out, diagnostic := fixture(t, bindings)
	app.in = strings.NewReader("private-fixture")
	args := []string{"secret", "put", "registry/key", "--repo", "origin/team/widget", "--file", ".oberth/build.yaml", "--step", "build"}
	if code := app.run(t.Context(), args); code != 0 || observedCalls(reflect.ValueOf(bindings)) != 2 {
		t.Fatal("private command is unreachable", code, diagnostic.String())
	}
	out.Reset()
	if app.run(t.Context(), []string{"help", "tool", row.definition.Name}) != 0 || !strings.Contains(out.String(), row.definition.Summary) {
		t.Fatal("private command missing help")
	}
}
