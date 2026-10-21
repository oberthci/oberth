package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
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
