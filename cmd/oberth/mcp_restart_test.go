package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/service"
)

func TestMCPReadReconnectAcrossRestart(t *testing.T) {
	for _, failure := range []string{"503", "reset", "SSE EOF", "handover"} {
		t.Run(failure, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var request struct {
					Params toolsCallParams `json:"params"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Error(err)
				}
				if string(request.Params.Arguments) != `{"run_id":"same-run"}` {
					t.Errorf("changed target: %s", raw)
				}
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("lost identity")
				}
				if calls.Add(1) == 1 {
					switch failure {
					case "503":
						w.WriteHeader(http.StatusServiceUnavailable)
					case "reset":
						conn, _, err := http.NewResponseController(w).Hijack()
						if err != nil {
							t.Error(err)
							return
						}
						conn.Close()
					case "SSE EOF":
						w.Header().Set("Content-Type", mcpContentTypeSSE)
						io.WriteString(w, ": keepalive\n\n")
					case "handover":
						testWriteJSONRPCError(w, nil, api.RestartingRPCCode, "server restarting")
					}
					return
				}
				id := 1
				testWriteJSONRPCResult(w, &id, map[string]string{"status": "passed"})
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			response, err := mcpRequest(t.Context(), mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: "fixture-token"}, timeout: time.Second}, mcpMethodToolsCall, toolsCallParams{Name: "run_get", Arguments: json.RawMessage(`{"run_id":"same-run"}`)})
			if err != nil || response == nil || response.Error != nil || calls.Load() != 2 {
				t.Fatalf("response=%+v err=%v calls=%d", response, err, calls.Load())
			}
		})
	}
}

func TestMCPNeverRetriesMutationsOrPermanentErrors(t *testing.T) {
	for _, name := range []string{"promote", "sync", "run_cancel", "admission_release", "issue_create", "future_tool", "status"} {
		for _, status := range []int{http.StatusServiceUnavailable, http.StatusForbidden, http.StatusBadRequest} {
			if name == "status" && status == http.StatusServiceUnavailable {
				continue
			}
			t.Run(name+http.StatusText(status), func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					io.Copy(io.Discard, r.Body)
					calls.Add(1)
					w.WriteHeader(status)
				}))
				defer server.Close()
				defer withTestMCPClient(t, server)()
				_, err := mcpRequest(t.Context(), mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL}, timeout: time.Second}, mcpMethodToolsCall, toolsCallParams{Name: name})
				if err == nil || calls.Load() != 1 {
					t.Fatalf("err=%v calls=%d", err, calls.Load())
				}
			})
		}
	}
}

// The reconnect allowlist is cross-checked against the server: every entry is
// a real tool outside the audit mutation gate, and no gated tool reconnects.
func TestMCPReconnectAllowlistIsReadOnly(t *testing.T) {
	tools := api.ToolNames()
	for name := range mcpReconnectableTools {
		if !slices.Contains(tools, name) {
			t.Errorf("reconnect allowlist names %q, which is not an MCP tool", name)
		}
		if service.MutatingTool(name) {
			t.Errorf("reconnect allowlist names mutation %q", name)
		}
	}
	for _, name := range tools {
		if service.MutatingTool(name) && mcpReadRequest(mcpMethodToolsCall, toolsCallParams{Name: name}) {
			t.Errorf("mutation %q reconnects", name)
		}
	}
}

func TestMCPReconnectShrinksLongPollTimeout(t *testing.T) {
	for _, test := range []struct {
		name    string
		timeout time.Duration
		want    func(seconds int64) bool
	}{
		// Derived deadline: 600s tool timeout + 30s margin; the retry keeps the margin.
		{name: "derived", want: func(seconds int64) bool { return seconds >= 590 && seconds < 600 }},
		// An explicit short client deadline still gets an answer before it expires.
		{name: "explicit", timeout: 4 * time.Second, want: func(seconds int64) bool { return seconds >= 1 && seconds < 4 }},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			var retried map[string]json.RawMessage
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var request struct {
					Params toolsCallParams `json:"params"`
				}
				if err := json.Unmarshal(raw, &request); err != nil {
					t.Error(err)
				}
				if calls.Add(1) == 1 {
					if string(request.Params.Arguments) != `{"sha":"abc","timeout":600}` {
						t.Errorf("first request changed: %s", request.Params.Arguments)
					}
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				if err := json.Unmarshal(request.Params.Arguments, &retried); err != nil {
					t.Error(err)
				}
				id := 1
				testWriteJSONRPCResult(w, &id, map[string]string{"status": "running"})
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			cfg := mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL}, timeout: test.timeout}
			_, err := mcpRequest(t.Context(), cfg, mcpMethodToolsCall, toolsCallParams{Name: "wait", Arguments: json.RawMessage(`{"sha":"abc","timeout":600}`)})
			if err != nil || calls.Load() != 2 {
				t.Fatalf("err=%v calls=%d", err, calls.Load())
			}
			var seconds int64
			if err := json.Unmarshal(retried["timeout"], &seconds); err != nil || !test.want(seconds) {
				t.Fatalf("retried timeout = %s (%v)", retried["timeout"], err)
			}
			if string(retried["sha"]) != `"abc"` || len(retried) != 2 {
				t.Fatalf("retry changed the target: %v", retried)
			}
		})
	}
}

func TestMCPRetryParamsKeepFittingAndOtherTools(t *testing.T) {
	read := toolsCallParams{Name: "run_get", Arguments: json.RawMessage(`{"id":"run","timeout":5}`)}
	if got := mcpRetryParams(read, 5*time.Second, time.Second); !slices.Equal(got.(toolsCallParams).Arguments, read.Arguments) {
		t.Fatalf("non-long-poll arguments changed: %s", got.(toolsCallParams).Arguments)
	}
	fits := toolsCallParams{Name: "promote_status", Arguments: json.RawMessage(`{"id":"p","timeout":60}`)}
	if got := mcpRetryParams(fits, time.Minute, 10*time.Minute); !slices.Equal(got.(toolsCallParams).Arguments, fits.Arguments) {
		t.Fatalf("fitting arguments changed: %s", got.(toolsCallParams).Arguments)
	}
	omitted := toolsCallParams{Name: "promote_status", Arguments: json.RawMessage(`{"id":"p"}`)}
	if got := mcpRetryParams(omitted, service.DefaultMaximumWait, 100*time.Second); string(got.(toolsCallParams).Arguments) != `{"id":"p","timeout":70}` {
		t.Fatalf("omitted timeout not bounded: %s", got.(toolsCallParams).Arguments)
	}
}

func TestMCPReconnectWindowFlag(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "fixture-token")
	cfg, err := parseMCPFlags("mcp call", []string{"--reconnect-window=5m", "status"}, -1)
	if err != nil || cfg.reconnectWindow != 5*time.Minute {
		t.Fatalf("cfg=%+v err=%v", cfg.reconnectWindow, err)
	}
	for _, value := range []string{"0s", "-1s", "soon"} {
		if _, err := parseMCPFlags("mcp call", []string{"--reconnect-window", value, "status"}, -1); !errors.Is(err, errUsage) {
			t.Fatalf("%s accepted: %v", value, err)
		}
	}
}

func TestMCPReconnectHonorsOriginalDeadline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	defer withTestMCPClient(t, server)()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	_, err := mcpRequest(ctx, mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL}}, mcpMethodToolsCall, toolsCallParams{Name: "wait"})
	if err == nil || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestMCPMutationDoesNotRetryLostReplyOrHandover(t *testing.T) {
	for _, handover := range []bool{false, true} {
		var calls atomic.Int32
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			io.Copy(io.Discard, r.Body)
			calls.Add(1) // Simulate the mutation having committed before its reply is lost.
			if handover {
				testWriteJSONRPCError(w, nil, api.RestartingRPCCode, "server restarting")
				return
			}
			connection, _, err := http.NewResponseController(w).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			connection.Close()
		}))
		restore := withTestMCPClient(t, server)
		response, err := mcpRequest(t.Context(), mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL}, timeout: time.Second}, mcpMethodToolsCall, toolsCallParams{Name: "promote"})
		restore()
		server.Close()
		if calls.Load() != 1 {
			t.Fatalf("committed mutation was replayed: %d", calls.Load())
		}
		if handover {
			if err != nil || response == nil || response.Error == nil {
				t.Fatalf("lost handover error: %+v %v", response, err)
			}
		} else if err == nil {
			t.Fatal("lost mutation reply treated as success")
		}
	}
}
