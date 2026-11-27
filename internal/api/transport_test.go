package api_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/contracttest"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type fixture struct {
	server      *api.Server
	bindings    apicontract.Bindings
	auth        *fake.Authenticator
	clock       *fake.Clock
	diagnostics *fake.Diagnostics
}

func admin() apicontract.Caller {
	return apicontract.Caller{Actor: actor.Actor{AuthMethod: "bearer", Identity: "operator", Fingerprint: "fingerprint", Admin: true}, CredentialID: "private-id"}
}
func setup(t *testing.T, change func(*api.Config, *apicontract.Bindings)) *fixture {
	t.Helper()
	b := fake.Bindings()
	clock := fake.NewClock()
	auth := fake.NewAuth(admin())
	diagnostics := &fake.Diagnostics{}
	c := api.Config{BrowserOrigin: os.Getenv("OBERTH_UI_ORIGIN"), FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil)), Metrics: func(context.Context, apicontract.Caller) (string, error) { return "oberth_ready 1\n", nil }, Auth: auth, Runtime: clock.Runtime(), Classify: publicerr.Classify, Diagnostics: diagnostics, Streams: &fake.Streams{}, Enroll: &fake.Handler[model.InviteRedemption, model.InviteCertificate]{}, Views: apicontract.Views{Counts: apicontract.Binding[apicontract.Empty, apicontract.RunCounts]{Handler: &fake.Handler[apicontract.Empty, apicontract.RunCounts]{}}, Containers: apicontract.Binding[apicontract.IDArgs, apicontract.ContainerPage]{Handler: &fake.Handler[apicontract.IDArgs, apicontract.ContainerPage]{Value: apicontract.ContainerPage{Containers: []apicontract.Container{}}}}, ContainerLog: apicontract.Binding[apicontract.ContainerArgs, apicontract.LogResult]{Handler: &fake.Handler[apicontract.ContainerArgs, apicontract.LogResult]{}}}}
	if change != nil {
		change(&c, &b)
	}
	registry, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Registry == nil {
		c.Registry = registry
	}
	s, err := api.New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.BeginDrain)
	return &fixture{s, b, auth, clock, diagnostics}
}
func request(f *fixture, method, target string, body []byte) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, bytes.NewReader(body))
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Mcp-Session-Id", "00000000000000000000000000000001")
	if target == "/mcp/enroll" {
		r.Header.Del("Authorization")
		r.Header.Del("Mcp-Session-Id")
	}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	return w
}
func rpc(f *fixture, name string, args any) *httptest.ResponseRecorder {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "request-1", "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	return request(f, "POST", "/mcp", body)
}
func assertSuccess(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != 200 || strings.Contains(w.Body.String(), `"isError":true`) || strings.Contains(w.Body.String(), `"error":`) {
		t.Fatalf("unexpected reply: %d %s", w.Code, w.Body.String())
	}
}

func TestToolRegistryAndDecodeContract(t *testing.T) {
	f := setup(t, nil)
	r, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	request(f, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`))
	tools := listTools(t, r, admin())
	if len(tools) != 75 {
		t.Fatalf("incomplete inventory: %d", len(tools))
	}
	for _, tool := range tools {
		t.Run(tool.Name, func(t *testing.T) {
			args := toolArgs(tool)
			assertSuccess(t, rpc(f, tool.Name, args))
			assertBindingReached(t, f.bindings, tool.Name)
			args["unknown_field"] = true
			w := rpc(f, tool.Name, args)
			if !strings.Contains(w.Body.String(), `"isError":true`) {
				t.Fatalf("unknown field reached handler: %s", w.Body.String())
			}
		})
	}
	if _, err := api.Builtin(apicontract.Bindings{}); err == nil {
		t.Fatal("unbound registry accepted")
	}
	if _, err := api.NewRegistry(nil); err == nil {
		t.Fatal("empty registry accepted")
	}
}
func toolArgs(m apicontract.Metadata) map[string]any {
	args := map[string]any{}
	properties := m.InputSchema["properties"].(apicontract.Schema)
	for _, key := range m.InputSchema["required"].([]string) {
		args[key] = schemaExample(properties[key].(apicontract.Schema))
	}
	if _, ok := properties["repo"]; ok {
		args["repo"] = "origin/team/widget"
	}
	switch m.Name {
	case "promote":
		args["sha"] = "12345678"
	case "wait", "promote_status", "run_wait":
		delete(args, "repo")
		args["id"] = "12345678"
	case "run_purge":
		args["what"] = "logs"
	case "issue_get_many", "issue_close_many":
		args["ids"] = []int64{1}
	case "issue_update":
		args["id"] = 1
	}
	return args
}
func TestStrictDecodeBeforePreparation(t *testing.T) {
	f := setup(t, nil)
	h := f.bindings.RunGet.Handler.(*fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord])
	for _, body := range []string{`[]`, `null`, `{"id":"12345678","bad":1}`, `{"id":42}`, `{"id":"12345678"} {}`, `{`} {
		w := request(f, "POST", "/api/admin/uplink-add", []byte(body))
		if w.Code == 200 {
			t.Fatalf("accepted invalid: %s", body)
		}
		w = rpc(f, "run_get", json.RawMessage(body))
		if w.Code == 200 && !strings.Contains(w.Body.String(), `"isError":true`) && !strings.Contains(w.Body.String(), `"error":`) {
			t.Fatalf("accepted invalid: %s", body)
		}
	}
	inputs, _, _ := h.Observed()
	if len(inputs) != 0 {
		t.Fatal("invalid input reached preparation")
	}
	assertSuccess(t, rpc(f, "system_status", nil))
	assertSuccess(t, rpc(f, "run_get", map[string]any{"id": "12345678", "step_limit": nil}))
	inputs, _, _ = h.Observed()
	if len(inputs) != 1 || inputs[0].StepLimit != 100 {
		t.Fatal("null did not take the typed default")
	}
}

func TestViewPreservesReasonRemedyAndDetail(t *testing.T) {
	detail := strings.Repeat("full native detail\n", 400)
	run := stepmodel.RunSummary{ID: "12345678", Status: stepmodel.RunRunning, DisplayReason: strings.Repeat("r", 40), Detail: detail}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: run}}
	})
	for _, w := range []*httptest.ResponseRecorder{request(f, "GET", "/api/runs/12345678", nil), rpc(f, "run_get", map[string]any{"id": "12345678"})} {
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), strings.Repeat("r", 40)) || !strings.Contains(w.Body.String(), `full native detail\n`) {
			t.Fatal("message lost")
		}
	}
	e := publicerr.New(publicerr.Conflict, strings.Repeat("r", 80), strings.Repeat("m", 120), errors.New("private cause"))
	e.Detail = detail
	e.Context = []string{"Required public context."}
	f = setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Error: e}
	})
	w := request(f, "GET", "/api/runs/12345678", nil)
	var got struct {
		Context                []string
		Reason, Remedy, Detail string
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Context) != 1 || got.Context[0] != e.Context[0] || got.Reason != e.Reason() || got.Remedy != e.Remedy() || got.Detail != detail || strings.Contains(w.Body.String(), "private cause") {
		t.Fatal("public error changed or leaked")
	}
}
func TestVisibilityAndReadOnlyIssueObservation(t *testing.T) {
	f := setup(t, nil)
	c := admin()
	c.Actor.Scope = []string{"origin"}
	f.auth.Set(c, nil)
	w := rpc(f, "uplink_add", map[string]any{"identity": "agent"})
	if !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("scoped administration accepted")
	}
	assertSuccess(t, request(f, "GET", "/api/issues/1", nil))
	h := f.bindings.IssueLock.Handler.(*fake.Handler[apicontract.IssueLockArgs, model.IssueLock])
	_, _, calls := h.Observed()
	if calls != 0 {
		t.Fatal("issue read renewed lock")
	}
}
func TestTypedHandlerContract(t *testing.T) {
	contracttest.Handler(t, contracttest.HandlerCase[apicontract.IDArgs, apicontract.Receipt]{New: func(*testing.T) apicontract.Handler[apicontract.IDArgs, apicontract.Receipt] {
		return &fake.Handler[apicontract.IDArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "12345678"}}
	}, Caller: admin(), Input: apicontract.IDArgs{ID: "12345678"}, Want: apicontract.Receipt{ID: "12345678"}})
}
func TestConditionalAndGzipReadsAuthenticateFirst(t *testing.T) {
	f := setup(t, nil)
	first := request(f, "GET", "/api/status", nil)
	assertSuccess(t, first)
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Accept-Encoding", "gzip")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	z, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(z)
	if err != nil {
		t.Fatal(err)
	}
	if err = z.Close(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(raw, first.Body.Bytes()) || w.Header().Get("ETag") != first.Header().Get("ETag") {
		t.Fatal("encoding changed representation or validator")
	}
	r.Header.Set("If-None-Match", first.Header().Get("ETag"))
	w = httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 304 {
		t.Fatal(w.Code)
	}
	r.Header.Del("Authorization")
	w = httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 401 {
		t.Fatal("conditional read bypassed auth")
	}
}
func TestAuthenticationTemporaryFailureAndDrain(t *testing.T) {
	f := setup(t, nil)
	f.auth.Set(apicontract.Caller{}, errors.New("private lookup failure"))
	w := request(f, "GET", "/api/status", nil)
	if w.Code != 503 || w.Header().Get("Retry-After") != "1" || strings.Contains(w.Body.String(), "private lookup") {
		t.Fatal(w.Body.String())
	}
	f.auth.Set(admin(), nil)
	f.server.BeginDrain()
	if w = request(f, "GET", "/readyz", nil); w.Code != 503 {
		t.Fatal("drain retained readiness")
	}
	if w = request(f, "GET", "/api/status", nil); w.Code != 503 {
		t.Fatal("drain accepted new work")
	}
}
func TestPublicReadRouteInventory(t *testing.T) {
	for _, route := range []string{"/api/runs", "/api/runs/counts", "/api/runs/12345678", "/api/runs/12345678/logs", "/api/runs/12345678/containers", "/api/runs/12345678/containerlog?node_id=node&container=main", "/api/repos", "/api/issues", "/api/issues/1", "/api/grants", "/api/audit", "/api/schedules", "/api/status", "/api/uplinks", "/metrics"} {
		t.Run(route, func(t *testing.T) {
			f := setup(t, nil)
			assertSuccess(t, request(f, "GET", route, nil))
			r := httptest.NewRequest("GET", route, nil)
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, modernRequest(r))
			if w.Code != 401 {
				t.Fatalf("unauthenticated route: %d", w.Code)
			}
		})
	}
}
func TestRunLogsDefaultsPreserveExplicitWindows(t *testing.T) {
	f := setup(t, nil)
	h := f.bindings.RunLogs.Handler.(*fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult])
	assertSuccess(t, rpc(f, "run_logs", map[string]any{"id": "12345678"}))
	assertSuccess(t, rpc(f, "run_logs", map[string]any{"id": "12345678", "limit": 0, "tail": false, "pattern": ""}))
	inputs, _, _ := h.Observed()
	if *inputs[0].Limit != 200 || !*inputs[0].Tail || *inputs[1].Limit != 0 || *inputs[1].Tail {
		t.Fatal("explicit filter changed")
	}
}
func TestUnknownErrorsHaveOnlyCorrelationReference(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Error: errors.New("private dependency credentials")}
	})
	w := rpc(f, "run_get", map[string]any{"id": "12345678"})
	if strings.Contains(w.Body.String(), "private dependency") || !strings.Contains(w.Body.String(), "Reference:") {
		t.Fatal(w.Body.String())
	}
}
func TestPrepareCarriesCallerForServiceAuditResolution(t *testing.T) {
	f := setup(t, nil)
	assertSuccess(t, rpc(f, "promote", map[string]any{"repo": "origin/team/widget", "sha": "12345678", "branch": "main"}))
	h := f.bindings.Promote.Handler.(*fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult])
	_, callers, calls := h.Observed()
	if calls != 1 || callers[0].AuditTarget != "" || !reflect.DeepEqual(callers[0].Actor, admin().Actor) {
		t.Fatal("typed target or live caller lost")
	}
}

func schemaExample(p apicontract.Schema) any {
	if variants, ok := p["anyOf"].([]apicontract.Schema); ok {
		p = variants[0]
	}
	switch p["type"] {
	case "integer":
		return int64(1)
	case "array":
		return []string{"value"}
	case "object":
		return map[string]any{"namespace": "execution", "service_account": "runner"}
	}
	return "value"
}

func TestDependencyContractSuites(t *testing.T) {
	sink := &fake.Diagnostics{}
	contracttest.Diagnostics(t, sink, sink.Items)
	contracttest.Authentication(t, contracttest.AuthenticationCase{New: func(*testing.T) api.Authenticator { return fake.NewAuth(admin()) }, Token: "valid", Caller: admin()})
	contracttest.Streams(t, contracttest.StreamsCase{New: func(*testing.T) api.Streams { return &fake.Streams{} }, Caller: admin(), Input: apicontract.ContainerArgs{ID: "12345678", NodeID: "node", Container: "main"}, Bytes: "native bytes\n"})
}
func TestNamedServiceMethodAdapterContract(t *testing.T) {
	contracttest.Handler(t, contracttest.HandlerCase[apicontract.IDArgs, apicontract.Receipt]{
		New: func(*testing.T) apicontract.Handler[apicontract.IDArgs, apicontract.Receipt] {
			return apicontract.HandlerFunc[apicontract.IDArgs, apicontract.Receipt](func(ctx context.Context, _ apicontract.Caller, input apicontract.IDArgs) (apicontract.Call[apicontract.Receipt], error) {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				return func(ctx context.Context) (apicontract.Receipt, error) {
					if err := ctx.Err(); err != nil {
						return apicontract.Receipt{}, err
					}
					return apicontract.Receipt{ID: input.ID}, nil
				}, nil
			})
		},
		Caller: admin(), Input: apicontract.IDArgs{ID: "12345678"}, Want: apicontract.Receipt{ID: "12345678"},
	})
}

func TestNonAdminRegistryVisibility(t *testing.T) {
	b := fake.Bindings()
	r, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	caller := admin()
	caller.Actor.Admin = false
	for _, m := range listTools(t, r, caller) {
		if m.Access != apicontract.Read {
			t.Fatal("administrator-only tool was visible")
		}
	}
	if _, err := r.Invoke(t.Context(), caller, "uplink_add", []byte(`{"identity":"agent"}`)); err == nil {
		t.Fatal("non-admin invoked administration")
	}
}
func TestRunLogsDefaultsRespectEachExplicitField(t *testing.T) {
	for key, value := range map[string]any{"pattern": "", "context": 0, "offset": 0, "byte_offset": 0, "limit": 0, "tail": false} {
		f := setup(t, nil)
		args := map[string]any{"id": "12345678", key: value}
		assertSuccess(t, rpc(f, "run_logs", args))
		h := f.bindings.RunLogs.Handler.(*fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult])
		inputs, _, _ := h.Observed()
		if inputs[0].Limit != nil && *inputs[0].Limit == 200 {
			t.Fatalf("%s was treated as omitted", key)
		}
	}
}

func assertBindingReached(t *testing.T, b apicontract.Bindings, name string) {
	t.Helper()
	field := ""
	for _, part := range strings.Split(name, "_") {
		field += strings.ToUpper(part[:1]) + part[1:]
	}
	method := reflect.ValueOf(b).FieldByName(field).FieldByName("Handler").Elem().MethodByName("Observed")
	if !method.IsValid() {
		return
	}
	result := method.Call(nil)
	if result[2].Int() != 1 {
		t.Fatalf("%s reached a different typed binding", name)
	}
}

func TestWaitPreservesSelectorsForServiceResolution(t *testing.T) {
	f := setup(t, nil)
	for _, args := range []map[string]any{{"sha": "12345678", "repo": "origin/team/widget", "trigger": "tag"}, {"ref": "main"}, {"id": "12345678"}} {
		assertSuccess(t, rpc(f, "wait", args))
	}
	h := f.bindings.Wait.Handler.(*fake.Handler[apicontract.WaitArgs, apicontract.WaitResult])
	inputs, _, _ := h.Observed()
	if len(inputs) != 3 || inputs[0].SHA != "12345678" || inputs[0].Trigger != "tag" {
		t.Fatal("wait identity was rewritten")
	}
}

func TestUnknownFailureReferenceMatchesItsFixedDiagnostic(t *testing.T) {
	for _, tool := range []string{"run_get", "run_wait"} {
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Error: errors.New("private dependency")}
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Error: errors.New("private dependency")}
		})
		w := rpc(f, tool, map[string]any{"id": "12345678"})
		items := f.diagnostics.Items()
		if len(items) != 1 || items[0].Tool != tool || !strings.Contains(w.Body.String(), "Reference: "+items[0].Reference) {
			t.Fatal("caller reference cannot be found in diagnostics")
		}
	}
}

func modernRequest(r *http.Request) *http.Request {
	if r.Method != "POST" || (r.URL.Path != "/mcp" && r.URL.Path != "/mcp/enroll") {
		return r
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return r
	}
	originalSize := len(raw)
	var wire apicontract.MCPRequest
	if json.Unmarshal(raw, &wire) == nil {
		params, err := apicontract.WithClientMetadata(wire.Params)
		if err == nil {
			wire.Params = params
			raw, _ = json.Marshal(wire)
			if len(raw) < originalSize {
				raw = append(raw, bytes.Repeat([]byte(" "), originalSize-len(raw))...)
			}
			for name, values := range wire.Headers() {
				r.Header[name] = values
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return r
}
