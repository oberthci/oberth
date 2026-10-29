package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type unmarshalableWaitResult struct{ Invalid chan int }

func (unmarshalableWaitResult) MCPToolText() string { return "fixed public text" }

type waitEvents struct {
	mu       sync.Mutex
	rows     []mcpWaitEvent
	terminal chan struct{}
}

func (events *waitEvents) record(value mcpWaitEvent) {
	events.mu.Lock()
	defer events.mu.Unlock()
	events.rows = append(events.rows, value)
	if value.Event == "terminal" && events.terminal != nil {
		close(events.terminal)
	}
}
func (events *waitEvents) check(t *testing.T, nonce, mode, outcome string, tools ...string) mcpWaitEvent {
	t.Helper()
	events.mu.Lock()
	defer events.mu.Unlock()
	if len(events.rows) != 2 {
		t.Fatalf("event count %d", len(events.rows))
	}
	first, last := events.rows[0], events.rows[1]
	tool := "wait"
	if len(tools) == 1 {
		tool = tools[0]
	}
	if first.Event != "start" || last.Event != "terminal" || first.Nonce != nonce || last.Nonce != nonce || !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(nonce) || first.Mode != mode || last.Mode != mode || last.Outcome != outcome || first.Tool != tool || last.Tool != tool || last.ElapsedMS < 0 {
		t.Fatalf("invalid fixed events: %+v", events.rows)
	}
	for _, event := range events.rows {
		if _, err := time.Parse(time.RFC3339Nano, event.UTC); err != nil {
			t.Fatal(err)
		}
	}
	raw, _ := json.Marshal(events.rows)
	for _, canary := range []string{"valid-token", "exact-request", "0123456789abcdef", "private-canary", "agent@host"} {
		if strings.Contains(string(raw), canary) {
			t.Fatal("private diagnostic canary")
		}
	}
	return last
}

func TestMCPWaitObservationTransportAndPrivacy(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, accept, wantMode, wantOutcome string
		invalid                             bool
	}{
		{"json", "application/json", "json", "final-frame-flushed", false},
		{"stream", "text/event-stream", "sse", "final-frame-flushed", false},
		{"refusal", "text/event-stream", "json", "preparation-refused", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
				if tc.invalid {
					return nil, errors.New("private-canary")
				}
				return func(context.Context) (any, error) { return nil, errors.New("private-canary") }, nil
			}}
			server := streamTestServer(t, backend)
			server.classifyError = func(error) (int, string) { return http.StatusInternalServerError, "private-canary" }
			events := &waitEvents{}
			server.mcpWaitLog = events.record
			writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
			server.Handler().ServeHTTP(writer, streamRequest(waitRPC, tc.accept, "valid-token"))
			last := events.check(t, writer.Header().Get("X-Oberth-Wait-ID"), tc.wantMode, tc.wantOutcome)
			if last.FinalFrames != 1 || strings.Contains(writer.Body.String(), "private-canary") {
				t.Fatal("response changed or leaked")
			}
			if !strings.Contains(writer.Body.String(), `"exact-request"`) {
				t.Fatal("RPC identity changed")
			}
		})
	}
}

func TestMCPWaitObservationExcludedRequests(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ body, token string }{
		{waitRPC, "invalid"},
		{`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait","extra":true}}`, "valid-token"},
		{strings.Replace(waitRPC, `"name":"wait"`, `"name":"status"`, 1), "valid-token"},
	} {
		server := streamTestServer(t, &streamingBackend{fakeBackend: &fakeBackend{}})
		events := &waitEvents{}
		server.mcpWaitLog = events.record
		writer := httptest.NewRecorder()
		server.Handler().ServeHTTP(writer, streamRequest(tc.body, "text/event-stream", tc.token))
		if len(events.rows) != 0 || writer.Header().Get("X-Oberth-Wait-ID") != "" {
			t.Fatal("excluded request observed")
		}
	}
}

func TestMCPWaitObservationWriteFailuresAndPanic(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, outcome             string
		failAt                    int
		block, panicCall, marshal bool
	}{
		{name: "initial", outcome: "initial-write-failed", failAt: 1},
		{name: "heartbeat", outcome: "heartbeat-write-failed", failAt: 2, block: true},
		{name: "final", outcome: "final-write-failed", failAt: 2},
		{name: "panic", outcome: "panic", panicCall: true},
		{name: "invalid-result", outcome: "final-frame-flushed", marshal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
				return func(ctx context.Context) (any, error) {
					called = true
					if tc.panicCall {
						panic("private-canary")
					}
					if tc.block {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					if tc.marshal {
						return unmarshalableWaitResult{Invalid: make(chan int)}, nil
					}
					return true, nil
				}, nil
			}}
			server := streamTestServer(t, backend)
			// Only the heartbeat case may see a heartbeat: in the others a fast
			// ticker can race the immediate result and fail its write first.
			server.mcpHeartbeatInterval = time.Hour
			if tc.block {
				server.mcpHeartbeatInterval = time.Millisecond
			}
			events := &waitEvents{}
			server.mcpWaitLog = events.record
			writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder(), failAt: tc.failAt}
			func() {
				defer func() {
					got := recover()
					if (got != nil) != tc.panicCall {
						t.Error("panic behavior changed")
					}
				}()
				server.Handler().ServeHTTP(writer, streamRequest(waitRPC, "text/event-stream", "valid-token"))
			}()
			events.check(t, writer.Header().Get("X-Oberth-Wait-ID"), "sse", tc.outcome)
			if tc.marshal {
				body := writer.Body.String()
				if !strings.Contains(body, `"isError":true`) || strings.Contains(body, `"structuredContent"`) || !strings.Contains(body, "tool result must be a JSON object") {
					t.Fatalf("invalid structured result must become a complete tool error: %s", body)
				}
			}
			if called != (tc.failAt != 1) {
				t.Fatal("observer invocation mismatch")
			}
			writes := writer.writes
			time.Sleep(5 * time.Millisecond)
			if writer.writes != writes {
				t.Fatal("writer survived terminal event")
			}
		})
	}
}

func TestMCPWaitObservationRealTLS(t *testing.T) {
	t.Parallel()
	for _, http2 := range []bool{false, true} {
		for _, disconnect := range []bool{false, true} {
			t.Run(map[bool]string{false: "HTTP1", true: "HTTP2"}[http2]+map[bool]string{false: "success", true: "disconnect"}[disconnect], func(t *testing.T) {
				release := make(chan struct{})
				observed := make(chan struct{})
				backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
					return func(ctx context.Context) (any, error) {
						defer close(observed)
						select {
						case <-release:
							return true, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}, nil
				}}
				server := streamTestServer(t, backend)
				events := &waitEvents{terminal: make(chan struct{})}
				server.mcpWaitLog = events.record
				tls := httptest.NewUnstartedServer(server.Handler())
				tls.EnableHTTP2 = http2
				tls.StartTLS()
				defer tls.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, tls.URL+"/mcp", strings.NewReader(waitRPC))
				if err != nil {
					t.Fatal(err)
				}
				req.Header.Set("Authorization", "Bearer valid-token")
				req.Header.Set("Accept", "text/event-stream")
				response, err := tls.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				if (response.ProtoMajor == 2) != http2 {
					t.Fatal("wrong transport")
				}
				reader := bufio.NewReader(response.Body)
				line, err := reader.ReadString('\n')
				if err != nil || line != ": keepalive\n" {
					t.Fatal("missing fixed heartbeat")
				}
				want := "final-frame-flushed"
				if disconnect {
					want = "request-context-canceled"
					cancel()
					_ = response.Body.Close()
				} else {
					close(release)
					if _, err = io.ReadAll(reader); err != nil {
						t.Fatal(err)
					}
				}
				select {
				case <-events.terminal:
				case <-time.After(time.Second):
					t.Fatal("missing terminal")
				}
				select {
				case <-observed:
				default:
					t.Fatal("observer not joined")
				}
				last := events.check(t, response.Header.Get("X-Oberth-Wait-ID"), "sse", want)
				if last.InitialFrames != 1 || (!disconnect && last.FinalFrames != 1) {
					t.Fatal("frame counts")
				}
			})
		}
	}
}

func TestMCPWaitObservationActualLoggerNoRawError(t *testing.T) {
	const child = "OBERTH_TEST_WAIT_LOG_CHILD"
	if os.Getenv(child) == "1" {
		backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
			return func(context.Context) (any, error) { return nil, errors.New("raw-secret-canary-do-not-log") }, nil
		}}
		server := streamTestServer(t, backend)
		server.classifyError = func(error) (int, string) { return http.StatusInternalServerError, "internal" }
		writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
		server.Handler().ServeHTTP(writer, streamRequest(waitRPC, "text/event-stream", "valid-token"))
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestMCPWaitObservationActualLoggerNoRawError$")
	command.Env = append(os.Environ(), child+"=1")
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("logger child failed: %v", err)
	}
	if strings.Contains(string(raw), "raw-secret-canary-do-not-log") || strings.Contains(string(raw), "exact-request") || strings.Count(string(raw), "mcp_wait ") != 2 {
		t.Fatal("unbounded/private legacy logger output")
	}
}

func TestMCPWaitObservationSocketDeadline(t *testing.T) {
	t.Parallel()
	for _, phase := range []string{"initial", "heartbeat", "final"} {
		t.Run(phase, func(t *testing.T) {
			conn, peer := net.Pipe()
			defer conn.Close()
			defer peer.Close()
			readerDone := make(chan struct{})
			if phase != "initial" {
				go func() { defer close(readerDone); _, _ = io.ReadFull(peer, make([]byte, len(": keepalive\n\n"))) }()
			} else {
				close(readerDone)
			}
			events := &waitEvents{}
			server := &Server{mcpHeartbeatInterval: time.Millisecond, mcpStreamWriteTimeout: 30 * time.Millisecond, mcpWaitLog: events.record}
			writer := &blockedFrameWriter{conn: conn, header: make(http.Header)}
			trace := server.beginMCPWait(writer, "wait", true)
			started := time.Now()
			err := server.streamLongPollObserved(writer, httptest.NewRequest(http.MethodPost, "/mcp", nil), rpcResponse{}, func(ctx context.Context) (any, error) {
				if phase == "final" {
					return true, nil
				}
				<-ctx.Done()
				return nil, ctx.Err()
			}, time.Now().Add(time.Minute), trace)
			trace.finish()
			if err == nil || time.Since(started) > time.Second {
				t.Fatal("deadline not preserved")
			}
			<-readerDone
			events.check(t, writer.Header().Get("X-Oberth-Wait-ID"), "sse", "write-deadline-exceeded")
		})
	}
}

func TestMCPWaitObservationPromotion(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"json", "sse"} {
		t.Run(mode, func(t *testing.T) {
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(_ context.Context, _ Actor, name string, _ json.RawMessage) (PreparedLongPoll, error) {
				if name != "promote_status" {
					t.Fatal("wrong preparation tool")
				}
				return func(context.Context) (any, error) { return map[string]bool{"still_running": true}, nil }, nil
			}}
			server := streamTestServer(t, backend)
			events := &waitEvents{}
			server.mcpWaitLog = events.record
			writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
			accept := "application/json"
			if mode == "sse" {
				accept = "text/event-stream"
			}
			body := strings.Replace(waitRPC, `"name":"wait"`, `"name":"promote_status"`, 1)
			server.Handler().ServeHTTP(writer, streamRequest(body, accept, "valid-token"))
			events.check(t, writer.Header().Get("X-Oberth-Wait-ID"), mode, "final-frame-flushed", "promote_status")
			if mode == "json" && backend.calledName != "promote_status" {
				t.Fatal("wrong JSON dispatch")
			}
		})
	}
}
