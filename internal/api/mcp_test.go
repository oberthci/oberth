package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestMCPRejectsOversizeBody verifies that the MaxBytesReader guard rejects a
// body larger than the configured limit. The server must return a parse error
// (the MaxBytesReader closes the connection with a 413, but the json.Decoder
// surfaces it as a decode error that handleMCP maps to a JSON-RPC parse
// error).
func TestMCPRejectsOversizeBody(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)

	// maxRequestBytes is 1 << 20 (1 MiB). Build a body just over the limit.
	oversizePayload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"` +
		strings.Repeat("x", (1<<20)+1) + `"}}}`

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(oversizePayload))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	// The handler must NOT have invoked the backend.
	if backend.calledName != "" {
		t.Fatalf("backend was invoked for oversize body: tool=%q", backend.calledName)
	}

	// The response must be a JSON-RPC parse error (code -32700).
	if response.Code != http.StatusOK {
		// Note: the MCP handler writes a 200 with a JSON-RPC error body rather
		// than a raw HTTP 413, because the MaxBytesReader error surfaces through
		// json.Decoder as a read error that handleMCP maps to parse error.
		t.Fatalf("status = %d, want 200 (JSON-RPC error envelope)", response.Code)
	}
	body := response.Body.String()
	if !strings.Contains(body, `"code":-32700`) || !strings.Contains(body, `"parse error"`) {
		t.Fatalf("oversize body response = %s, want JSON-RPC parse error", body)
	}
}

// TestMCPAcceptsJustUnderLimitBody verifies that a body just under the
// MaxBytesReader limit still parses correctly.
func TestMCPAcceptsJustUnderLimitBody(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)

	// Build a valid JSON-RPC request whose total size is under 1 MiB.
	// The ref value is padded to make the body close to (but under) the limit.
	// We use a conservative size to avoid off-by-one with JSON overhead.
	padSize := (1 << 20) - 256 // well under the limit but large
	validPayload := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"` +
		strings.Repeat("a", padSize) + `"}}}`

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(validPayload))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	// The body must not be a parse error — the backend should have been called.
	if strings.Contains(body, `"code":-32700`) {
		t.Fatalf("just-under-limit body was rejected as parse error: %s", body)
	}
	if backend.calledName != "status" {
		t.Fatalf("backend.calledName = %q, want %q", backend.calledName, "status")
	}
}

// TestMCPToolErrorClassification verifies that MCP tool errors are classified
// through the same ErrorClassifier as the dashboard API, so raw internal error
// chains (sqlite context, file paths) never reach the client.
func TestMCPToolErrorClassification(t *testing.T) {
	t.Parallel()

	// sentinel errors that mirror the service/store sentinels
	errInvalidInput := errors.New("service: invalid input")
	errNotFound := errors.New("store: not found")

	classifier := func(err error) (int, string) {
		switch {
		case errors.Is(err, errInvalidInput):
			return http.StatusBadRequest, err.Error()
		case errors.Is(err, errNotFound):
			return http.StatusNotFound, "not found"
		default:
			return http.StatusInternalServerError, "internal error"
		}
	}

	cases := map[string]struct {
		err          error
		wantContains string
		wantAbsent   string
	}{
		"sqlite-flavored internal error": {
			err:          fmt.Errorf("read audit chain head: %w", errors.New("sqlite: database is locked (5) (SQLITE_BUSY)")),
			wantContains: "internal error (reference ",
			wantAbsent:   "sqlite",
		},
		"actionable invalid input": {
			err:          fmt.Errorf("%w: ref is required", errInvalidInput),
			wantContains: "ref is required",
		},
		"actionable not found": {
			err:          fmt.Errorf("%w: run selector \"abc1234\"", errNotFound),
			wantContains: "not found",
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			backend := &fakeBackend{toolErr: tc.err}
			server, err := New(backend, backend, backend, "test", WithErrorClassifier(classifier))
			if err != nil {
				t.Fatal(err)
			}
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"main"}}}`
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", response.Code)
			}
			respBody := response.Body.String()
			if !strings.Contains(respBody, `"isError":true`) {
				t.Fatalf("response missing isError:true: %s", respBody)
			}
			if !strings.Contains(respBody, tc.wantContains) {
				t.Fatalf("response %q does not contain %q", respBody, tc.wantContains)
			}
			if tc.wantAbsent != "" && strings.Contains(respBody, tc.wantAbsent) {
				t.Fatalf("response %q should not contain %q", respBody, tc.wantAbsent)
			}
		})
	}
}

// TestMCPAndDashboardClassifyIdentically verifies parity between the MCP
// and dashboard error classification paths for a representative error set.
func TestMCPAndDashboardClassifyIdentically(t *testing.T) {
	t.Parallel()

	errInvalidInput := errors.New("service: invalid input")
	errNotFound := errors.New("store: not found")
	errForbidden := errors.New("service: forbidden")

	classifier := func(err error) (int, string) {
		switch {
		case errors.Is(err, errInvalidInput):
			return http.StatusBadRequest, err.Error()
		case errors.Is(err, errNotFound):
			return http.StatusNotFound, "not found"
		case errors.Is(err, errForbidden):
			return http.StatusForbidden, "forbidden"
		default:
			return http.StatusInternalServerError, "internal error"
		}
	}

	errs := []error{
		fmt.Errorf("%w: ref is required", errInvalidInput),
		fmt.Errorf("%w: run selector \"abc\"", errNotFound),
		fmt.Errorf("%w: admin only", errForbidden),
		errors.New("unexpected internal state: corrupted index"),
	}

	for _, testErr := range errs {
		t.Run(testErr.Error(), func(t *testing.T) {
			dashboardCode, dashboardMsg := classifier(testErr)

			// MCP path: the classified message is what toolFailure returns
			backend := &fakeBackend{toolErr: testErr}
			server, err := New(backend, backend, backend, "test", WithErrorClassifier(classifier))
			if err != nil {
				t.Fatal(err)
			}
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"status","arguments":{"ref":"main"}}}`
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			respBody := response.Body.String()
			if dashboardCode == http.StatusInternalServerError {
				// MCP should return a generic message with a correlation ID, not the raw error
				if strings.Contains(respBody, testErr.Error()) {
					t.Fatalf("MCP leaked internal error chain: %s", respBody)
				}
				if !strings.Contains(respBody, "internal error (reference ") {
					t.Fatalf("MCP missing generic error with reference: %s", respBody)
				}
			} else {
				// Actionable errors should contain the dashboard message
				if !strings.Contains(respBody, dashboardMsg) {
					t.Fatalf("MCP response %q does not contain dashboard message %q", respBody, dashboardMsg)
				}
			}
		})
	}
}

// TestMCPWriteDeadlineExceedsWaitCeiling verifies that the write-deadline
// margin added for long-poll tools exceeds the maximum tool wait duration,
// so raising the service ceiling without updating the HTTP layer cannot
// reintroduce #789.
func TestMCPWriteDeadlineExceedsWaitCeiling(t *testing.T) {
	t.Parallel()
	ceiling := 10 * time.Minute // matches the default used by New
	server, _ := testServer(t)
	total := server.maximumToolWait + writeDeadlineMargin
	if total <= ceiling {
		t.Fatalf("write deadline %v must exceed wait ceiling %v", total, ceiling)
	}
}

// TestMCPWriteDeadlineDerivesFromConfiguredCeiling verifies that
// WithMaximumToolWait propagates to the write-deadline extension so the
// margin is always relative to the configured ceiling, never a literal.
func TestMCPWriteDeadlineDerivesFromConfiguredCeiling(t *testing.T) {
	t.Parallel()
	custom := 20 * time.Minute
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test", WithMaximumToolWait(custom))
	if err != nil {
		t.Fatal(err)
	}
	if server.maximumToolWait != custom {
		t.Fatalf("maximumToolWait = %v, want %v", server.maximumToolWait, custom)
	}
	total := server.maximumToolWait + writeDeadlineMargin
	if total <= custom {
		t.Fatalf("write deadline %v must exceed wait ceiling %v", total, custom)
	}
}

// TestMCPRunListReturnsObject verifies that run_list structuredContent is a
// JSON object with a "runs" key, not a bare array (#794).
func TestMCPRunListReturnsObject(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	backend.toolResult = RunListResponse{Runs: []map[string]string{{"id": "run-1"}}}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"run_list","arguments":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.IsError {
		t.Fatalf("run_list returned error: %s", response.Body.String())
	}
	// structuredContent must be a JSON object, not an array.
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Result.Structured, &parsed); err != nil {
		t.Fatalf("structuredContent is not a JSON object: %s", string(envelope.Result.Structured))
	}
	if _, ok := parsed["runs"]; !ok {
		t.Fatalf("structuredContent missing 'runs' key: %s", string(envelope.Result.Structured))
	}
}

// TestMCPRepoListReturnsObject verifies that repo_list structuredContent is a
// JSON object with a "repositories" key, not a bare array (#794).
func TestMCPRepoListReturnsObject(t *testing.T) {
	t.Parallel()
	server, backend := testServer(t)
	backend.toolResult = RepoListResponse{Repositories: []map[string]string{{"name": "oberth"}}}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"repo_list","arguments":{}}}`
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result struct {
			Structured json.RawMessage `json:"structuredContent"`
			IsError    bool            `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result.IsError {
		t.Fatalf("repo_list returned error: %s", response.Body.String())
	}
	var parsed map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Result.Structured, &parsed); err != nil {
		t.Fatalf("structuredContent is not a JSON object: %s", string(envelope.Result.Structured))
	}
	if _, ok := parsed["repositories"]; !ok {
		t.Fatalf("structuredContent missing 'repositories' key: %s", string(envelope.Result.Structured))
	}
}

// TestMCPAllToolsReturnObjectStructuredContent verifies that every
// registered MCP tool produces a JSON object (not array, not scalar)
// in structuredContent for a representative successful call (#794).
// NOTE: this test exercises the fakeBackend, which always returns an
// object. The service-level shape test in internal/service/mcp_shape_test.go
// exercises the real API.CallTool and is the mutation-evidence test for #794.
func TestMCPAllToolsReturnObjectStructuredContent(t *testing.T) {
	t.Parallel()
	server, _ := testServer(t)
	// Tools that are known to return objects by construction. The
	// test sends a tools/call for each and asserts the structuredContent
	// is a JSON object.
	for _, tool := range []struct {
		name string
		args string
	}{
		{"status", `{"ref":"main"}`},
		{"run_list", `{}`},
		{"repo_list", `{}`},
		{"system_status", `{}`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, tool.name, tool.args)
			request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
			request.Header.Set("Authorization", "Bearer valid-token")
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)

			var envelope struct {
				Result struct {
					Structured json.RawMessage `json:"structuredContent"`
					IsError    bool            `json:"isError"`
				} `json:"result"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode response: %v body=%s", err, response.Body.String())
			}
			if len(envelope.Result.Structured) == 0 {
				// Tool errors have no structuredContent; skip.
				if envelope.Result.IsError {
					return
				}
				t.Fatalf("non-error response has empty structuredContent")
			}
			// Must be a JSON object (starts with '{'), not an array or scalar.
			trimmed := strings.TrimSpace(string(envelope.Result.Structured))
			if trimmed[0] != '{' {
				t.Fatalf("structuredContent is not a JSON object: %s", trimmed[:min(80, len(trimmed))])
			}
		})
	}
}

// TestMCPWriteTimeoutRegressionLongPoll verifies that a long-poll tool
// (wait) completes successfully even when the HTTP server's WriteTimeout
// is shorter than the tool's execution time. This is the regression test
// for #789/#793: without the SetWriteDeadline extension in handleMCP, the
// server's WriteTimeout terminates the response mid-stream.
func TestMCPWriteTimeoutRegressionLongPoll(t *testing.T) {
	t.Parallel()

	// Backend that sleeps on "wait" calls, simulating a long-poll.
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test", WithMaximumToolWait(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}

	// Start a real HTTP server with a short WriteTimeout on a loopback
	// listener. If SetWriteDeadline is not extended for long-poll tools,
	// the WriteTimeout fires and truncates the response.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()

	httpServer := &http.Server{
		Handler:      server.Handler(),
		WriteTimeout: 200 * time.Millisecond,
	}
	go func() { _ = httpServer.Serve(ln) }()
	defer func() { _ = httpServer.Close() }()

	// The backend returns after 400ms, which exceeds WriteTimeout.
	backend.toolResult = map[string]string{"status": "terminal", "sha": strings.Repeat("a", 40)}
	origCallTool := backend.CallTool
	_ = origCallTool
	// Override CallTool to add a sleep for "wait" tool.
	slowBackend := &slowWaitBackend{fakeBackend: backend, delay: 400 * time.Millisecond}
	server2, err := New(slowBackend, slowBackend, slowBackend, "test", WithMaximumToolWait(10*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	httpServer2 := &http.Server{
		Handler:      server2.Handler(),
		WriteTimeout: 200 * time.Millisecond,
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln2.Close() }()
	go func() { _ = httpServer2.Serve(ln2) }()
	defer func() { _ = httpServer2.Close() }()

	// Make a request for "wait" which will exceed WriteTimeout.
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait","arguments":{"sha":"` + strings.Repeat("a", 40) + `"}}}`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+ln2.Addr().String()+"/mcp", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer valid-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v (WriteTimeout likely truncated the response)", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", resp.StatusCode, string(respBody))
	}

	// The response must be a complete JSON-RPC 2.0 response.
	var envelope rpcResponse
	if err := json.Unmarshal(respBody, &envelope); err != nil {
		t.Fatalf("response is not valid JSON (truncated by WriteTimeout?): %v body=%s", err, string(respBody))
	}
	if envelope.JSONRPC != "2.0" || envelope.Error != nil {
		t.Fatalf("response = %#v, want complete JSON-RPC 2.0 success", envelope)
	}
}

// slowWaitBackend wraps fakeBackend and adds a delay only for "wait" calls.
type slowWaitBackend struct {
	*fakeBackend
	delay time.Duration
}

func (s *slowWaitBackend) CallTool(ctx context.Context, actor Actor, name string, arguments json.RawMessage) (any, error) {
	if name == "wait" {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return s.fakeBackend.CallTool(ctx, actor, name, arguments)
}
