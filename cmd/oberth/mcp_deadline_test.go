package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestMCPDeadlineDerivesToolTimeoutAndHonorsOverride(t *testing.T) {
	for _, test := range []struct {
		name     string
		tool     string
		args     string
		override time.Duration
		want     time.Duration
	}{
		{"ordinary", "status", `{}`, 0, time.Minute},
		{"short wait", "wait", `{"timeout":30}`, 0, time.Minute},
		{"long wait", "wait", `{"timeout":600}`, 0, 630 * time.Second},
		{"default wait", "wait", `{}`, 0, 630 * time.Second},
		{"absent arguments", "wait", ``, 0, 630 * time.Second},
		{"zero wait", "wait", `{"timeout":0}`, 0, 630 * time.Second},
		{"default promotion", "promote_status", `{}`, 0, 630 * time.Second},
		{"generic tool", "custom_poll", `{"timeout":120}`, 0, 150 * time.Second},
		{"override shorter", "wait", `{"timeout":600}`, time.Second, time.Second},
		{"override longer", "status", `{}`, 2 * time.Minute, 2 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			limit, _, err := mcpDeadline(mcpFlagResult{timeout: test.override}, toolsCallParams{Name: test.tool, Arguments: json.RawMessage(test.args)})
			if err != nil || limit != test.want {
				t.Fatalf("deadline = %s, %v; want %s", limit, err, test.want)
			}
		})
	}
	if _, _, err := mcpDeadline(mcpFlagResult{}, toolsCallParams{Name: "wait", Arguments: json.RawMessage(`{"timeout":9223372036854775807}`)}); !errors.Is(err, errUsage) {
		t.Fatalf("unrepresentable deadline accepted: %v", err)
	}
}

func TestMCPTimeoutFlag(t *testing.T) {
	t.Setenv("OBERTH_TOKEN", "deadline-test-token")
	for _, args := range [][]string{{"--timeout", "90s"}, {"--timeout=90s"}} {
		cfg, err := parseMCPFlags("mcp call", args, 0)
		if err != nil || cfg.timeout != 90*time.Second {
			t.Fatalf("timeout flag = %s, %v", cfg.timeout, err)
		}
	}
	for _, value := range []string{"", "0", "-1s", "invalid", "99999999999999999h"} {
		if _, err := parseMCPFlags("mcp call", []string{"--timeout=" + value}, 0); !errors.Is(err, errUsage) {
			t.Fatalf("accepted timeout %q: %v", value, err)
		}
	}
}

// Exercise the actual CLI HTTP/TLS/SSE path beyond both former 60-second
// limits. In-memory connections let synctest advance the full minute virtually.
func TestMCPCallStreamsHeartbeatsBeyondSixtySeconds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var heartbeats atomic.Int64
		server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request jsonRPCRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Error(err)
				return
			}
			w.Header().Set("Content-Type", mcpContentTypeSSE)
			finished := time.NewTimer(61 * time.Second)
			defer finished.Stop()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if _, err := fmt.Fprint(w, ": keepalive\n\n"); err != nil {
					return
				}
				if err := http.NewResponseController(w).Flush(); err != nil {
					t.Error(err)
					return
				}
				heartbeats.Add(1)
				select {
				case <-r.Context().Done():
					return
				case <-ticker.C:
					continue
				case <-finished.C:
					payload, err := json.Marshal(jsonRPCResponse{JSONRPC: mcpJSONRPCVersion, ID: &request.ID, Result: json.RawMessage(`{"content":[{"type":"text","text":"finished"}]}`)})
					if err != nil {
						t.Error(err)
						return
					}
					_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
					return
				}
			}
		}))
		// Replace the TCP listener before starting TLS; all network waits then
		// belong to the synctest bubble, including the client's timeout timer.
		_ = server.Listener.Close()
		listener := &mcpPipeListener{connections: make(chan net.Conn), closed: make(chan struct{})}
		server.Listener = listener
		server.StartTLS()
		defer server.Close()
		original := mcpHTTPClientFunc
		client := server.Client()
		client.Transport.(*http.Transport).DialContext = listener.dialContext
		client.Timeout = mcpRequestTimeout
		mcpHTTPClientFunc = func(string) (*http.Client, error) { return client, nil }
		defer func() { mcpHTTPClientFunc = original }()
		t.Setenv("OBERTH_TOKEN", "heartbeat-test-token")
		var output strings.Builder
		started := time.Now()
		err := runMCPCall(t.Context(), []string{"custom_poll", `{"timeout":300}`, "--url", server.URL}, &output)
		if err != nil || output.String() != "finished\n" {
			t.Fatalf("streaming call = %q, %v", output.String(), err)
		}
		if elapsed := time.Since(started); elapsed != 61*time.Second {
			t.Fatalf("stream duration = %s, want 61s", elapsed)
		}
		if heartbeats.Load() < 60 {
			t.Fatal("long-running tool did not stream heartbeats")
		}
		if client.Timeout != mcpRequestTimeout {
			t.Fatal("shared HTTP client was mutated")
		}
	})
}

func TestMCPCallDeadlineErrorNamesLimit(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				if stream {
					w.Header().Set("Content-Type", mcpContentTypeSSE)
					_, _ = fmt.Fprint(w, ": keepalive\n\n")
					_ = http.NewResponseController(w).Flush()
				}
				<-r.Context().Done()
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			t.Setenv("OBERTH_TOKEN", "deadline-test-token")
			var output strings.Builder
			err := runMCPCall(t.Context(), []string{"wait", `{"timeout":600}`, "--url", server.URL, "--timeout=30ms"}, &output)
			if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "client deadline 30ms") || !strings.Contains(err.Error(), "tool timeout 10m0s") {
				t.Fatalf("deadline diagnostic = %v", err)
			}
			if output.Len() != 0 {
				t.Fatalf("deadline printed partial output: %q", output.String())
			}
		})
	}
}

func TestMCPDeadlineErrorPreservesCauseAndReadError(t *testing.T) {
	for _, readErr := range []error{io.EOF, errors.New("SSE stream contained no data events"), context.DeadlineExceeded} {
		for _, toolTimeout := range []time.Duration{0, 10 * time.Minute} {
			err := mcpDeadlineError(time.Minute, toolTimeout, readErr)
			if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, readErr) {
				t.Fatalf("deadline lost cause or read error: %v", err)
			}
			if strings.Count(err.Error(), context.DeadlineExceeded.Error()) != 1 {
				t.Fatalf("deadline cause repeated: %v", err)
			}
		}
	}
}

func TestMCPRequestPreservesCallerCancellation(t *testing.T) {
	server := newMCPTestServer(t, "cancel-test-token")
	defer server.Close()
	defer withTestMCPClient(t, server)()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := mcpRequest(ctx, mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: "cancel-test-token"}}, mcpMethodToolsCall, toolsCallParams{Name: "wait"})
	if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "client deadline") {
		t.Fatalf("caller cancellation = %v", err)
	}
}

func TestMCPRequestPreservesEarlierCallerDeadline(t *testing.T) {
	server := newMCPTestServer(t, "caller-deadline-token")
	defer server.Close()
	defer withTestMCPClient(t, server)()
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := mcpRequest(ctx, mcpFlagResult{mcpConfig: mcpConfig{URL: server.URL, Token: "caller-deadline-token"}}, mcpMethodToolsCall, toolsCallParams{Name: "wait"})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "client deadline") {
		t.Fatalf("caller deadline = %v", err)
	}
}

func TestMCPArtifactDownloadHonorsTimeout(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", "artifact-deadline-token")
	destination := filepath.Join(t.TempDir(), "artifact.bin")
	err := runMCPCall(t.Context(), []string{"artifact_get", `{"id":"run1","name":"file"}`, "--url", server.URL + "/mcp", "--output", destination, "--timeout=30ms"}, io.Discard)
	if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "client deadline 30ms") {
		t.Fatalf("artifact deadline = %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("timed-out artifact retained: %v", err)
	}
}

func TestMCPDeadlineTransportDoesNotExposeURL(t *testing.T) {
	err := classifyMCPTransport(&url.Error{Op: "Post", URL: "https://example.org/mcp?credential=private-fixture", Err: context.DeadlineExceeded})
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "private-fixture") {
		t.Fatalf("timeout classification = %v", err)
	}
}

// mcpPipeListener runs real HTTP/TLS over bubble-local, synchronous pipes.
// No kernel socket or wall-clock scheduling is needed for deadline coverage.
type mcpPipeListener struct {
	connections chan net.Conn
	closed      chan struct{}
	once        sync.Once
}

func (listener *mcpPipeListener) Accept() (net.Conn, error) {
	select {
	case conn := <-listener.connections:
		return conn, nil
	case <-listener.closed:
		return nil, net.ErrClosed
	}
}
func (listener *mcpPipeListener) Close() error {
	listener.once.Do(func() { close(listener.closed) })
	return nil
}
func (*mcpPipeListener) Addr() net.Addr { return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 443} }
func (listener *mcpPipeListener) dialContext(ctx context.Context, _, _ string) (net.Conn, error) {
	client, server := net.Pipe()
	select {
	case listener.connections <- server:
		return client, nil
	case <-ctx.Done():
		_ = client.Close()
		_ = server.Close()
		return nil, ctx.Err()
	}
}
