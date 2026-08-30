package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/runlog"
)

type fakeBackend struct {
	publishedRun    string
	publishErr      error
	calledName      string
	calledArguments json.RawMessage
	calledActor     Actor
	readyErr        error
	issueErr        error
	issueFilter     IssueFilter
	issueCalls      int
	toolResult      any
	toolErr         error
	runFilter       RunFilter
	runID           string
	logBurn         string
	logStep         string
	logCalls        int
	logFilter       runlog.Filter
	liveOffset      int64
	liveCalls       int
	artifactName    string
	artifactFilter  runlog.Filter
	artifactBody    string
	artifactErr     error
}

func (backend *fakeBackend) Authenticate(_ context.Context, token string) (Actor, error) {
	if token != "valid-token" {
		return Actor{}, errors.New("invalid")
	}
	return Actor{Identity: "agent@host", Fingerprint: "SHA256:key"}, nil
}

func (backend *fakeBackend) CallTool(_ context.Context, actor Actor, name string, arguments json.RawMessage) (any, error) {
	backend.calledName, backend.calledArguments, backend.calledActor = name, arguments, actor
	if backend.toolErr != nil {
		return nil, backend.toolErr
	}
	if backend.toolResult != nil {
		return backend.toolResult, nil
	}
	return map[string]string{"status": "running"}, nil
}

type rawToolResult struct {
	Output string `json:"output"`
}

func (result rawToolResult) MCPToolText() string { return result.Output }

func (backend *fakeBackend) Runs(_ context.Context, _ Actor, filter RunFilter) (any, error) {
	backend.runFilter = filter
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
func (backend *fakeBackend) RunArtifacts(_ context.Context, _ Actor, id string) (any, error) {
	backend.runID = id
	return map[string]any{"run_id": id, "artifacts": []map[string]any{{"name": "surefire/TEST.xml", "size": 12}}}, nil
}

func (backend *fakeBackend) RunArtifact(_ context.Context, _ Actor, id, name string, filter runlog.Filter) (any, error) {
	backend.runID, backend.artifactName, backend.artifactFilter = id, name, filter
	if backend.artifactErr != nil {
		return nil, backend.artifactErr
	}
	return fakeArtifact{name: name, body: backend.artifactBody}, nil
}

type fakeArtifact struct {
	name string
	body string
}

func (f fakeArtifact) ArtifactBytes() []byte { return []byte(f.body) }
func (f fakeArtifact) ArtifactName() string  { return f.name }

func (backend *fakeBackend) RunLogFiltered(_ context.Context, _ Actor, id, burn, step string, filter runlog.Filter) (any, error) {
	backend.runID, backend.logBurn, backend.logStep = id, burn, step
	backend.logFilter = filter
	backend.logCalls++
	return map[string]any{"run_id": id, "burn": burn, "step": step, "output": "[test/unit] ok\n"}, nil
}
func (backend *fakeBackend) RunLogLive(_ context.Context, _ Actor, id string, offset int64) (any, error) {
	backend.runID, backend.liveOffset = id, offset
	backend.liveCalls++
	return map[string]any{"run_id": id, "status": "running", "chunk": "line\n", "offset": offset + 5, "size": offset + 5, "done": false}, nil
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

func (backend *fakeBackend) PublishRun(context.Context, Actor, string) error {
	backend.publishedRun = "recorded"
	return backend.publishErr
}

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

func TestMCPCallAcceptsStandardRequestMetadata(t *testing.T) {
	server, backend := testServer(t)
	body := `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"status","arguments":{"repo":"oberth","ref":"main"},"_meta":{"progressToken":0,"threadId":"019c1234-5678-7000-8000-000000000001","x-codex-turn-metadata":{"model":"gpt-5.6-codex","reasoning_effort":"xhigh","turn_started_at_unix_ms":1786412400000}}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	var envelope rpcResponse
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || envelope.Error != nil {
		t.Fatalf("response = status %d, error %#v, body %s", response.Code, envelope.Error, response.Body.String())
	}
	if backend.calledName != "status" || string(backend.calledArguments) != `{"repo":"oberth","ref":"main"}` {
		t.Fatalf("backend call = %q arguments %s", backend.calledName, backend.calledArguments)
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
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{},"_meta":"not-an-object"}}`,
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

func TestMCPDeregisteredPlanApplyToolsAreUnknown(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	for _, tool := range []string{"plan", "plan_status", "apply_status"} {
		backend.calledName = ""
		body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + tool + `","arguments":{}}}`
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if backend.calledName != "" {
			t.Fatalf("backend invoked for de-registered tool %s", tool)
		}
		var envelope struct {
			Result struct {
				IsError bool              `json:"isError"`
				Content []json.RawMessage `json:"content"`
			} `json:"result"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
			t.Fatalf("decode %s response: %v", tool, err)
		}
		if !envelope.Result.IsError {
			t.Fatalf("%s should be an error response", tool)
		}
		if !strings.Contains(string(envelope.Result.Content[0]), "unknown tool") {
			t.Fatalf("%s error = %s, want 'unknown tool'", tool, string(envelope.Result.Content[0]))
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

func TestLiveLogViewRequiresAuthAndBoundsOffset(t *testing.T) {
	server, backend := testServer(t)
	unauthenticated := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/api/runs/run-1/livelog", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated live log = %d", unauthenticated.Code)
	}
	// The default offset is -1 (start tailing).
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/livelog", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || backend.runID != "run-1" || backend.liveOffset != -1 {
		t.Fatalf("live log = %d, forwarded id %q offset %d", response.Code, backend.runID, backend.liveOffset)
	}
	// An explicit cursor is forwarded exactly.
	cursor := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/livelog?offset=8192", nil)
	cursor.Header.Set("Authorization", "Bearer valid-token")
	cursorResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(cursorResponse, cursor)
	if cursorResponse.Code != http.StatusOK || backend.liveOffset != 8192 {
		t.Fatalf("live log cursor = %d, forwarded offset %d", cursorResponse.Code, backend.liveOffset)
	}
	backend.liveCalls = 0
	for _, query := range []string{"offset=-2", "offset=abc", "offset=1.5", "offset=9223372036854775808"} {
		malformed := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/livelog?"+query, nil)
		malformed.Header.Set("Authorization", "Bearer valid-token")
		rejected := httptest.NewRecorder()
		server.Handler().ServeHTTP(rejected, malformed)
		if rejected.Code != http.StatusBadRequest {
			t.Fatalf("live log query %q = %d, want 400", query, rejected.Code)
		}
	}
	if backend.liveCalls != 0 {
		t.Fatalf("malformed live offsets reached the backend %d times", backend.liveCalls)
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

func TestRunsViewCarriesRepoAndRefFilters(t *testing.T) {
	server, backend := testServer(t)
	request := httptest.NewRequest(http.MethodGet, "/api/runs?repo=oberth&ref=fix/dashboard-strip-log-prefix&limit=5", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	want := RunFilter{Repository: "oberth", Ref: "fix/dashboard-strip-log-prefix", Limit: 5}
	if backend.runFilter != want {
		t.Fatalf("run filter = %#v, want %#v", backend.runFilter, want)
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

func TestWriteViewUsesInjectedClassifier(t *testing.T) {
	t.Parallel()
	classifier := func(err error) (int, string) {
		if err.Error() == "classified" {
			return http.StatusNotFound, "not found"
		}
		return http.StatusTeapot, "unclassified"
	}
	backend := &fakeBackend{}
	backend.issueErr = errors.New("classified")
	server, err := New(backend, backend, backend, "test", WithErrorClassifier(classifier))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", response.Code)
	}
	// Unclassified errors
	backend.issueErr = errors.New("surprise")
	response2 := httptest.NewRecorder()
	request2 := httptest.NewRequest(http.MethodGet, "/api/issues", nil)
	request2.Header.Set("Authorization", "Bearer valid-token")
	server.Handler().ServeHTTP(response2, request2)
	if response2.Code != http.StatusTeapot {
		t.Fatalf("unclassified status = %d, want 418", response2.Code)
	}
}

func TestDefaultClassifierReturns500(t *testing.T) {
	t.Parallel()
	code, msg := defaultClassifyError(errors.New("anything"))
	if code != http.StatusInternalServerError || msg != "internal error" {
		t.Fatalf("default = %d %q", code, msg)
	}
}

func TestMCPToolInputsMatchContract(t *testing.T) {
	t.Parallel()
	want := map[string][]string{
		"status":     {"ref", "repo"},
		"logs":       {"context", "limit", "offset", "pattern", "repo", "sha", "step", "tail"},
		"run_get":    {"id"},
		"run_logs":   {"burn", "context", "id", "limit", "offset", "pattern", "step", "tail"},
		"wait":       {"repo", "sha", "timeout", "trigger"},
		"sync":       {"branch", "repo", "sha"},
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
			t.Fatalf("%s properties = %v, want exact contract inputs %v", name, got, expected)
		}
		delete(want, name)
	}
	if len(want) != 0 {
		t.Fatalf("tools missing input checks: %v", want)
	}
}

func TestIssueCreateRequiresOnlyContractArguments(t *testing.T) {
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
			t.Fatalf("issue_create properties = %#v, want only contract title/body", schema["properties"])
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

	issue := decode(t, IssueResponse{ID: 7, Kind: "ci", Title: "red", Body: "failed", State: "open",
		RepoID: 1, Branch: "main", Occurrences: 3, CIOrigin: "branch", CIWorkID: "run-1",
		CreatedAt: "2026-08-15T12:00:00Z", UpdatedAt: "2026-08-15T12:01:00Z"})
	// Core fields must always be present; optional fields appear via omitempty.
	for _, required := range []string{"id", "kind", "title", "body", "state"} {
		if _, ok := issue[required]; !ok {
			t.Fatalf("issue response missing required field %q: %v", required, issue)
		}
	}
	page := decode(t, IssueListResponse{Issues: []IssueListItem{{ID: 7, State: "open", Kind: "ci", Title: "red",
		RepoID: 1, Branch: "main", Occurrences: 1, CreatedAt: "2026-08-15T12:00:00Z", UpdatedAt: "2026-08-15T12:01:00Z"}}, NextBefore: 7})
	if got, want := keys(page), []string{"issues", "next_before"}; !slices.Equal(got, want) {
		t.Fatalf("issue list fields = %v, want %v", got, want)
	}
	items, ok := page["issues"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("issue list items = %#v", page["issues"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("issue list item = %#v", items[0])
	}
	// id and state are the minimum required fields
	for _, required := range []string{"id", "state"} {
		if _, ok := item[required]; !ok {
			t.Fatalf("issue list item missing required field %q: %v", required, item)
		}
	}
	lock := decode(t, IssueLockResponse{ID: 7, Owner: "agent@host", ExpiresAt: "2026-08-05T12:00:00Z", Renewed: true})
	if got, want := keys(lock), []string{"expires_at", "id", "owner", "renewed"}; !slices.Equal(got, want) {
		t.Fatalf("issue lock fields = %v, want %v", got, want)
	}
}

func TestMCPToolCountMatchesDocumented(t *testing.T) {
	t.Parallel()
	// The documented count in docs/mcp-setup.md must match the registered
	// tool count. A mismatch means the table drifted from the code.
	const documented = 24
	definitions := toolDefinitions()
	if len(definitions) != documented {
		t.Fatalf("registered %d tools, documented %d in docs/mcp-setup.md — update the table", len(definitions), documented)
	}

	// AGENT-CONTRACT.md must state the same count (issue #53 item 3).
	contract, err := os.ReadFile("../../AGENT-CONTRACT.md")
	if err != nil {
		t.Fatalf("read AGENT-CONTRACT.md: %v", err)
	}
	expected := fmt.Sprintf("MCP exposes %d tools", len(definitions))
	if !strings.Contains(string(contract), expected) {
		t.Fatalf("AGENT-CONTRACT.md does not contain %q — tool count drifted from code", expected)
	}
}

func TestMCPToolSurfaceMatchesContract(t *testing.T) {
	want := []string{
		"status", "logs", "run_get", "artifacts", "artifact_get", "run_logs", "wait", "sync", "promote", "promote_status",
		"issue_create", "issue_get", "issue_update", "issue_close",
		"issue_delete", "issue_list", "issue_lock",
		"access_list", "access_allow", "access_revoke",
		"repo_list", "repo_remove", "run_list", "system_status",
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
		t.Fatalf("MCP tools = %v, want exact MCP tool surface %v", got, want)
	}
}

func TestRunLogRejectsMalformedFilterParametersWithoutReachingTheView(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"offset=-1", "limit=abc", "context=999", "pattern=" + strings.Repeat("x", 600)} {
		server, backend := testServer(t)
		request := httptest.NewRequest(http.MethodGet, "/api/runs/r-1/logs?burn=test&step=unit&"+query, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("%s: status = %d, want 400", query, response.Code)
		}
		if backend.logCalls != 0 {
			t.Fatalf("%s: reached the view layer despite invalid input", query)
		}
	}
}

func TestRunLogPassesFilterParametersThrough(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	request := httptest.NewRequest(http.MethodGet,
		"/api/runs/r-1/logs?burn=test&step=unit&pattern=FAIL&context=2&offset=5&limit=10&tail=true", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", response.Code, response.Body.String())
	}
	want := runlog.Filter{Pattern: "FAIL", Context: 2, Offset: 5, Limit: 10, Tail: true}
	if backend.logFilter != want {
		t.Fatalf("filter = %#v, want %#v", backend.logFilter, want)
	}
}

func TestRunsLimitClampedToStoreMaximum(t *testing.T) {
	server, backend := testServer(t)

	// Request limit=500 — the handler must clamp it to 100.
	request := httptest.NewRequest(http.MethodGet, "/api/runs?limit=500", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if backend.runFilter.Limit != 100 {
		t.Fatalf("runs limit = %d, want 100 (store max)", backend.runFilter.Limit)
	}

	// Request limit=50 — below the cap, should pass through.
	request = httptest.NewRequest(http.MethodGet, "/api/runs?limit=50", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response = httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d", response.Code)
	}
	if backend.runFilter.Limit != 50 {
		t.Fatalf("runs limit = %d, want 50", backend.runFilter.Limit)
	}
}
