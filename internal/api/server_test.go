package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

type fakeBackend struct {
	calledName  string
	calledActor Actor
	readyErr    error
	issueErr    error
	issueFilter IssueFilter
	issueCalls  int
	toolResult  any
	runID       string
	logBurn     string
	logStep     string
	logCalls    int
}

func (backend *fakeBackend) Authenticate(_ context.Context, token string) (Actor, error) {
	if token != "valid-token" {
		return Actor{}, errors.New("invalid")
	}
	return Actor{Identity: "agent@host", Fingerprint: "SHA256:key"}, nil
}

func (backend *fakeBackend) CallTool(_ context.Context, actor Actor, name string, _ json.RawMessage) (any, error) {
	backend.calledName, backend.calledActor = name, actor
	if backend.toolResult != nil {
		return backend.toolResult, nil
	}
	return map[string]string{"status": "running"}, nil
}

type rawToolResult struct {
	Output string `json:"output"`
}

func (result rawToolResult) MCPToolText() string { return result.Output }

func (backend *fakeBackend) Runs(context.Context, Actor, int) (any, error) {
	return map[string]any{"runs": []any{}}, nil
}
func (backend *fakeBackend) Run(_ context.Context, _ Actor, id string) (any, error) {
	backend.runID = id
	return map[string]any{"Run": map[string]any{"ID": id}, "Steps": []any{}}, nil
}
func (backend *fakeBackend) RunLog(_ context.Context, _ Actor, id, burn, step string) (any, error) {
	backend.runID, backend.logBurn, backend.logStep = id, burn, step
	backend.logCalls++
	return map[string]string{"run_id": id, "burn": burn, "step": step, "output": "[test/unit] ok\n"}, nil
}
func (backend *fakeBackend) Repositories(context.Context, Actor) (any, error) {
	return map[string]any{"repositories": []any{}}, nil
}
func (backend *fakeBackend) Issues(_ context.Context, _ Actor, filter IssueFilter) (any, error) {
	backend.issueCalls++
	backend.issueFilter = filter
	return map[string]any{"issues": []any{}}, backend.issueErr
}
func (backend *fakeBackend) Status(context.Context, Actor) (any, error) {
	return map[string]string{"vcs": "ok", "cluster": "ok"}, nil
}
func (backend *fakeBackend) Ready(context.Context) error { return backend.readyErr }

func testServer(t *testing.T) (*Server, *fakeBackend) {
	t.Helper()
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	return server, backend
}

func TestAPIRequiresExactlyOneValidBearerToken(t *testing.T) {
	server, _ := testServer(t)
	for name, configure := range map[string]func(*http.Request){
		"missing": func(*http.Request) {},
		"wrong":   func(request *http.Request) { request.Header.Set("Authorization", "Bearer wrong") },
		"duplicate": func(request *http.Request) {
			request.Header.Add("Authorization", "Bearer valid-token")
			request.Header.Add("Authorization", "Bearer valid-token")
		},
	} {
		t.Run(name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/api/runs", nil)
			configure(request)
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d", response.Code)
			}
		})
	}
}

func TestMCPCallCarriesAuthenticatedUplinkIdentity(t *testing.T) {
	server, backend := testServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"feature/fab"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || backend.calledName != "status" || backend.calledActor.Identity != "agent@host" {
		t.Fatalf("response=%d call=%q actor=%#v body=%s", response.Code, backend.calledName, backend.calledActor, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), `"isError":false`) {
		t.Fatalf("response = %s", response.Body.String())
	}
}

func TestMCPLogContentIsExactRawSlice(t *testing.T) {
	server, backend := testServer(t)
	const output = "[test/setup] first\n[test/unit] second\n"
	backend.toolResult = rawToolResult{Output: output}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"logs","arguments":{"sha":"0123456789abcdef0123456789abcdef01234567","step":"test"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured rawToolResult `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Text != output || envelope.Result.Structured.Output != output {
		t.Fatalf("MCP log response = %#v", envelope.Result)
	}
}

func TestMCPInitializeAdvertisesAgentReadableDashboardState(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t)
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("initialize status = %d", response.Code)
	}
	var decoded struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{"/api/runs", "/api/repos", "/api/issues", "/api/status"} {
		if !strings.Contains(decoded.Result.Instructions, route) {
			t.Fatalf("initialize instructions omit %s: %q", route, decoded.Result.Instructions)
		}
	}
	if strings.Contains(strings.ToLower(decoded.Result.Instructions), "protected") {
		t.Fatalf("initialize instructions retained old branch doctrine: %q", decoded.Result.Instructions)
	}
}

func TestMCPPromoteResponseContainsOnlyDurableID(t *testing.T) {
	server, backend := testServer(t)
	backend.toolResult = PromoteResponse{ID: "promotion-0123456789abcdef"}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"promote","arguments":{"sha":"0123456789abcdef0123456789abcdef01234567","branch":"main"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured map[string]json.RawMessage `json:"structuredContent"`
			IsError    bool                       `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("promote response = status %d %#v", response.Code, envelope.Result)
	}
	if envelope.Result.Content[0].Text != `{"id":"promotion-0123456789abcdef"}` {
		t.Fatalf("promote text = %q", envelope.Result.Content[0].Text)
	}
	if len(envelope.Result.Structured) != 1 || string(envelope.Result.Structured["id"]) != `"promotion-0123456789abcdef"` {
		t.Fatalf("promote structured content = %#v", envelope.Result.Structured)
	}
}

func TestMCPRejectsUnknownFieldsAndUnknownToolsWithoutInvokingBackend(t *testing.T) {
	server, backend := testServer(t)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{},"extra":true}}`,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"shell","arguments":{}}}`,
	} {
		backend.calledName = ""
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if backend.calledName != "" {
			t.Fatalf("backend called for %s", body)
		}
	}
}

func TestUIUsesStaticScriptAndSecurityHeaders(t *testing.T) {
	server, _ := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/issues", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `data-view="issues"`) {
		t.Fatalf("status/body = %d %s", response.Code, response.Body.String())
	}
	if policy := response.Header().Get("Content-Security-Policy"); strings.Contains(policy, "unsafe-inline") || !strings.Contains(policy, "default-src 'none'") {
		t.Fatalf("CSP = %q", policy)
	}
	if body := response.Body.String(); !strings.Contains(body, `data-version="test"`) || !strings.Contains(body, "/assets/app.js") || !strings.Contains(body, "/assets/app.css") {
		t.Fatalf("dashboard shell missing version or asset references: %s", body)
	}
}

func TestDashboardShellCoversRunDetailDeepLinks(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/runs/run-0123456789abcdef", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `data-view="runs"`) {
		t.Fatalf("run deep link shell = %d %s", response.Code, response.Body.String())
	}
	if backend.runID != "" {
		t.Fatalf("serving the shell must not touch the view backend, got run lookup %q", backend.runID)
	}
}

func TestDashboardAssetsServeWithExplicitTypes(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t)
	for _, testCase := range []struct {
		path     string
		kind     string
		fragment string
	}{
		{"/assets/app.css", "text/css; charset=utf-8", ".pill"},
		{"/assets/app.js", "text/javascript; charset=utf-8", "oberth-token"},
		{"/assets/fonts/IBMPlexMono-Regular.woff2", "font/woff2", ""},
	} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, testCase.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", testCase.path, response.Code)
		}
		if kind := response.Header().Get("Content-Type"); kind != testCase.kind {
			t.Fatalf("%s content type = %q, want %q", testCase.path, kind, testCase.kind)
		}
		if testCase.fragment != "" && !strings.Contains(response.Body.String(), testCase.fragment) {
			t.Fatalf("%s is missing %q", testCase.path, testCase.fragment)
		}
	}
	for _, path := range []string{"/assets/app.txt", "/assets/../server.go", "/assets/missing.css"} {
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code == http.StatusOK {
			t.Fatalf("%s must not be served", path)
		}
	}
}

func TestRunDetailAndLogViewsRequireAuthAndForwardSelectors(t *testing.T) {
	server, backend := testServer(t)
	unauthenticated := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated run detail = %d", unauthenticated.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || backend.runID != "run-1" {
		t.Fatalf("run detail = %d, forwarded id %q", response.Code, backend.runID)
	}
	logRequest := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/logs?burn=test&step=unit", nil)
	logRequest.Header.Set("Authorization", "Bearer valid-token")
	logResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(logResponse, logRequest)
	if logResponse.Code != http.StatusOK || backend.logBurn != "test" || backend.logStep != "unit" {
		t.Fatalf("run log = %d, forwarded %q/%q", logResponse.Code, backend.logBurn, backend.logStep)
	}
	backend.logCalls = 0
	for _, query := range []string{"", "burn=test", "step=unit", "burn=%20&step=unit"} {
		malformed := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/logs?"+query, nil)
		malformed.Header.Set("Authorization", "Bearer valid-token")
		rejected := httptest.NewRecorder()
		server.Handler().ServeHTTP(rejected, malformed)
		if rejected.Code != http.StatusBadRequest {
			t.Fatalf("log query %q = %d, want 400", query, rejected.Code)
		}
	}
	if backend.logCalls != 0 {
		t.Fatalf("malformed log selectors reached the backend %d times", backend.logCalls)
	}
}

func TestReadinessFailsClosed(t *testing.T) {
	server, backend := testServer(t)
	backend.readyErr = errors.New("database unavailable")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", response.Code)
	}
}

func TestIssueViewCarriesUnifiedFilters(t *testing.T) {
	server, backend := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/issues?repo=oberth&kind=ci&state=open&before=42&limit=500", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	want := IssueFilter{Repository: "oberth", Kind: "ci", State: "open", BeforeID: 42, Limit: 50}
	if backend.issueFilter != want {
		t.Fatalf("issue filter = %#v, want %#v", backend.issueFilter, want)
	}
}

func TestIssueViewRejectsMalformedFiltersBeforeStorage(t *testing.T) {
	for _, query := range []string{"kind=bogus", "state=bogus", "before=abc", "before=-1", "limit=abc", "limit=0"} {
		t.Run(query, func(t *testing.T) {
			server, backend := testServer(t)
			request := httptest.NewRequest(http.MethodGet, "/api/issues?"+query, nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", response.Code, response.Body.String())
			}
			if backend.issueCalls != 0 {
				t.Fatalf("storage invoked %d times", backend.issueCalls)
			}
		})
	}
}

func TestIssueViewKeepsBackendFailuresAsServerErrors(t *testing.T) {
	server, backend := testServer(t)
	backend.issueErr = errors.New("database unavailable")
	request := httptest.NewRequest(http.MethodGet, "/api/issues?kind=ci", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusInternalServerError || backend.issueCalls != 1 {
		t.Fatalf("status = %d, calls = %d", response.Code, backend.issueCalls)
	}
}

func TestMCPToolInputsMatchFAB(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"status":     {"ref", "repo"},
		"logs":       {"repo", "sha", "step"},
		"wait":       {"repo", "sha", "timeout"},
		"sync":       {"repo", "sha"},
		"promote":    {"branch", "repo", "sha"},
		"issue_list": {"before"},
	}
	for _, definition := range toolDefinitions() {
		name, ok := definition["name"].(string)
		expected, selected := want[name]
		if !ok || !selected {
			continue
		}
		schema, ok := definition["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("%s schema = %#v", name, definition["inputSchema"])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s properties = %#v", name, schema["properties"])
		}
		got := make([]string, 0, len(properties))
		for property := range properties {
			got = append(got, property)
		}
		slices.Sort(got)
		if !slices.Equal(got, expected) {
			t.Fatalf("%s properties = %v, want exact FAB inputs %v", name, got, expected)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("tools missing input checks: %v", want)
	}
}

func TestIssueCreateRequiresOnlyFABArguments(t *testing.T) {
	t.Parallel()
	for _, definition := range toolDefinitions() {
		if definition["name"] != "issue_create" {
			continue
		}
		schema, ok := definition["inputSchema"].(map[string]any)
		if !ok {
			t.Fatalf("issue_create schema = %#v", definition["inputSchema"])
		}
		required, ok := schema["required"].([]string)
		if !ok || !slices.Equal(required, []string{"title", "body"}) {
			t.Fatalf("issue_create required fields = %#v, want exact title/body", schema["required"])
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok || properties["repo"] != nil {
			t.Fatalf("issue_create properties = %#v, want only FAB title/body", schema["properties"])
		}
		return
	}
	t.Fatal("issue_create tool not found")
}

func TestMCPCreateIssueResponseContainsOnlyDurableID(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	backend.toolResult = IssueCreateResponse{ID: 42}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_create","arguments":{"title":"follow up","body":"details"}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Structured map[string]any `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Result.Structured) != 1 || envelope.Result.Structured["id"] != float64(42) {
		t.Fatalf("issue_create response = %#v", envelope.Result.Structured)
	}
}

func TestMCPIssueResponsesUseMinimalWireShapes(t *testing.T) {
	t.Parallel()
	decode := func(t *testing.T, value any) map[string]any {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		return decoded
	}
	keys := func(value map[string]any) []string {
		result := make([]string, 0, len(value))
		for key := range value {
			result = append(result, key)
		}
		slices.Sort(result)
		return result
	}

	issue := decode(t, IssueResponse{ID: 7, Kind: "ci", Title: "red", Body: "failed", State: "open"})
	if got, want := keys(issue), []string{"body", "id", "kind", "state", "title"}; !slices.Equal(got, want) {
		t.Fatalf("issue response fields = %v, want %v", got, want)
	}
	page := decode(t, IssueListResponse{Issues: []IssueListItem{{ID: 7, State: "open"}}, NextBefore: 7})
	if got, want := keys(page), []string{"issues", "next_before"}; !slices.Equal(got, want) {
		t.Fatalf("issue list fields = %v, want %v", got, want)
	}
	items, ok := page["issues"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("issue list items = %#v", page["issues"])
	}
	item, ok := items[0].(map[string]any)
	if !ok || !slices.Equal(keys(item), []string{"id", "state"}) {
		t.Fatalf("issue list item = %#v", items[0])
	}
	lock := decode(t, IssueLockResponse{ID: 7, Owner: "agent@host", ExpiresAt: "2026-08-05T12:00:00Z"})
	if got, want := keys(lock), []string{"expires_at", "id", "owner"}; !slices.Equal(got, want) {
		t.Fatalf("issue lock fields = %v, want %v", got, want)
	}
}

func TestMCPToolSurfaceMatchesFAB(t *testing.T) {
	want := []string{
		"status", "logs", "wait", "sync", "promote", "promote_status",
		"issue_create", "issue_get", "issue_update", "issue_close",
		"issue_delete", "issue_list", "issue_lock",
	}
	definitions := toolDefinitions()
	got := make([]string, 0, len(definitions))
	for _, definition := range definitions {
		name, ok := definition["name"].(string)
		if !ok {
			t.Fatalf("tool name = %#v", definition["name"])
		}
		got = append(got, name)
	}
	if !slices.Equal(got, want) {
		t.Fatalf("MCP tools = %v, want exact FAB surface %v", got, want)
	}
}
