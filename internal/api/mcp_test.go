package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
