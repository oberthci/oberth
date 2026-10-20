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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type streamingBackend struct {
	*fakeBackend
	prepare func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error)
}

func (backend *streamingBackend) PrepareLongPoll(ctx context.Context, actor Actor, name string, raw json.RawMessage) (PreparedLongPoll, error) {
	return backend.prepare(ctx, actor, name, raw)
}

func TestMCPStreamAcceptNegotiation(t *testing.T) {
	t.Parallel()
	cases := []struct {
		values []string
		want   bool
	}{
		{nil, false}, {[]string{"application/json"}, false}, {[]string{"*/*"}, false},
		{[]string{"application/json, text/event-stream"}, true},
		{[]string{"application/json;q=1", "text/event-stream;q=0.1"}, true},
		{[]string{"text/event-stream;q=0", "text/event-stream"}, false},
		{[]string{"text/event-stream;q=0, */*;q=1"}, false},
		{[]string{"text/event-stream;q=garbage"}, false},
		{[]string{"text/event-stream;q=NaN"}, false},
		{[]string{"text/event-stream;q=1.1"}, false},
		{[]string{"text/event-streaming"}, false},
	}
	for _, tc := range cases {
		h := http.Header{}
		for _, v := range tc.values {
			h.Add("Accept", v)
		}
		if got := acceptsMCPStream(h); got != tc.want {
			t.Errorf("Accept %q: %v, want %v", tc.values, got, tc.want)
		}
	}
}

func streamTestServer(t *testing.T, backend *streamingBackend) *Server {
	t.Helper()
	server, err := New(backend, backend, backend, "test", WithErrorClassifier(func(error) (int, string) { return http.StatusBadRequest, "invalid observation" }))
	if err != nil {
		t.Fatal(err)
	}
	server.mcpHeartbeatInterval = 10 * time.Millisecond
	server.mcpStreamWriteTimeout = 100 * time.Millisecond
	return server
}

func streamRequest(body, accept, token string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	req.Header.Set("Accept", accept)
	req.Header.Set("Authorization", "Bearer "+token)
	return req
}

const waitRPC = `{"jsonrpc":"2.0","id":"exact-request","method":"tools/call","params":{"name":"wait","arguments":{"sha":"0123456789abcdef","timeout":300}}}`

func TestMCPStreamValidationAndCompatibility(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, body, accept, token string
		prepare                   bool
	}{
		{"unauthenticated", waitRPC, "text/event-stream", "invalid", false},
		{"oversize", strings.Repeat("x", maxRequestBytes+1), "text/event-stream", "valid-token", false},
		{"invalid envelope", `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"wait","extra":true}}`, "text/event-stream", "valid-token", false},
		{"invalid arguments", waitRPC, "text/event-stream", "valid-token", true},
		{"JSON compatibility", waitRPC, "application/json", "valid-token", false},
		{"excluded SSE", waitRPC, "application/json, text/event-stream;q=0", "valid-token", false},
		{"headerless", waitRPC, "", "valid-token", false},
		{"ordinary tool", strings.Replace(waitRPC, `"name":"wait"`, `"name":"status"`, 1), "text/event-stream", "valid-token", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
				called = true
				return nil, errors.New("private lookup detail")
			}}
			server := streamTestServer(t, backend)
			recorder := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
			server.Handler().ServeHTTP(recorder, streamRequest(tc.body, tc.accept, tc.token))
			if called != tc.prepare {
				t.Fatalf("prepared=%v want%v", called, tc.prepare)
			}
			if strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") || strings.Contains(recorder.Body.String(), "keepalive") {
				t.Fatal("stream began before validation or without negotiation")
			}
			if strings.Contains(recorder.Body.String(), "private lookup detail") {
				t.Fatal("private error leaked")
			}
		})
	}
}

// Real TLS HTTP/1.1 and HTTP/2 exercise ResponseController through the server's
// deadline-preserving middleware. Frames must arrive while the observer blocks;
// waiting until the result before flushing would reproduce the proxy failure.
func TestMCPStreamTLSHeartbeatAndFinalRPC(t *testing.T) {
	t.Parallel()
	for _, http2 := range []bool{false, true} {
		t.Run(map[bool]string{false: "HTTP1", true: "HTTP2"}[http2], func(t *testing.T) {
			var preparations atomic.Int32
			release := make(chan struct{})
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(_ context.Context, actor Actor, name string, _ json.RawMessage) (PreparedLongPoll, error) {
				preparations.Add(1)
				if actor.Identity != "agent@host" || name != "wait" {
					return nil, errors.New("identity/tool mismatch")
				}
				return func(ctx context.Context) (any, error) {
					select {
					case <-release:
						return map[string]any{"sha": "0123456789abcdef", "still_running": true}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}, nil
			}}
			server := streamTestServer(t, backend)
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
			req.Header.Set("Accept", "application/json, text/event-stream")
			resp, err := tls.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if (resp.ProtoMajor == 2) != http2 {
				t.Fatalf("protocol %s", resp.Proto)
			}
			if resp.Header.Get("Content-Type") != "text/event-stream" {
				t.Fatalf("content type %s", resp.Header.Get("Content-Type"))
			}
			reader := bufio.NewReader(resp.Body)
			for range 3 {
				for _, want := range []string{": keepalive\n", "\n"} {
					got, err := reader.ReadString('\n')
					if err != nil || got != want {
						t.Fatalf("heartbeat=%q err=%v", got, err)
					}
				}
			}
			close(release)
			tail, err := io.ReadAll(reader)
			if err != nil {
				t.Fatal(err)
			}
			text := string(tail)
			if strings.Count(text, "event: message\n") != 1 || strings.Count(text, "data: ") != 1 {
				t.Fatalf("final event: %s", text)
			}
			_, data, _ := strings.Cut(text, "data: ")
			data = strings.TrimSpace(data)
			var result rpcResponse
			if err := json.Unmarshal([]byte(data), &result); err != nil {
				t.Fatal(err)
			}
			if string(result.ID) != `"exact-request"` || result.JSONRPC != "2.0" || !strings.Contains(data, `"still_running":true`) {
				t.Fatalf("RPC mismatch: %s", data)
			}
			if preparations.Load() != 1 || backend.calledName != "" {
				t.Fatal("duplicate observation")
			}
		})
	}
}

type streamFailureWriter struct {
	*httptest.ResponseRecorder
	writes    int
	failAt    int
	failFlush bool
	deadlines []time.Time
}

func (w *streamFailureWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *streamFailureWriter) Write(p []byte) (int, error) {
	w.writes++
	if w.writes == w.failAt {
		return 0, io.ErrClosedPipe
	}
	return w.ResponseRecorder.Write(p)
}
func (w *streamFailureWriter) FlushError() error {
	if w.failFlush {
		return io.ErrClosedPipe
	}
	w.Flush()
	return nil
}
func (w *streamFailureWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func TestMCPStreamWriteFailureCancelsObserverAndJoins(t *testing.T) {
	t.Parallel()
	for _, failAt := range []int{1, 2, 3} {
		t.Run(string(rune('0'+failAt)), func(t *testing.T) {
			server := &Server{mcpHeartbeatInterval: time.Millisecond, mcpStreamWriteTimeout: time.Second}
			writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder(), failAt: failAt}
			called := false
			prepared := func(ctx context.Context) (any, error) { called = true; <-ctx.Done(); return nil, ctx.Err() }
			if failAt == 3 {
				prepared = func(context.Context) (any, error) { called = true; return map[string]bool{"done": true}, nil }
				writer.failAt = 2
			}
			deadline := time.Now().Add(time.Minute)
			err := server.streamLongPoll(writer, httptest.NewRequest(http.MethodPost, "/mcp", nil), rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("1")}, prepared, deadline)
			if !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("error %v", err)
			}
			if called != (failAt != 1) {
				t.Fatalf("called=%v", called)
			}
			if !writer.deadlines[len(writer.deadlines)-1].Equal(deadline) {
				t.Fatal("overall deadline not restored")
			}
			writes := writer.writes
			time.Sleep(5 * time.Millisecond)
			if writer.writes != writes {
				t.Fatal("writer survived handler")
			}
		})
	}
}

func TestMCPStreamFlushFailureAndPanicJoin(t *testing.T) {
	t.Parallel()
	server := &Server{mcpHeartbeatInterval: time.Millisecond}
	writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder(), failFlush: true}
	err := server.streamLongPoll(writer, httptest.NewRequest(http.MethodPost, "/mcp", nil), rpcResponse{}, func(context.Context) (any, error) { t.Fatal("observer started after flush failure"); return nil, nil }, time.Now().Add(time.Minute))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	writer = &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("missing observer panic")
			}
		}()
		_ = server.streamLongPoll(writer, httptest.NewRequest(http.MethodPost, "/mcp", nil), rpcResponse{}, func(context.Context) (any, error) { panic("observer") }, time.Now().Add(time.Minute))
	}()
	writes := writer.writes
	time.Sleep(5 * time.Millisecond)
	if writer.writes != writes {
		t.Fatal("heartbeat survived panic")
	}
}

func TestMCPStreamClientDisconnectStopsOnlyObserver(t *testing.T) {
	t.Parallel()
	observed := make(chan struct{})
	backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
		return func(ctx context.Context) (any, error) { defer close(observed); <-ctx.Done(); return nil, ctx.Err() }, nil
	}}
	server := streamTestServer(t, backend)
	tls := httptest.NewTLSServer(server.Handler())
	defer tls.Close()
	req, _ := http.NewRequest(http.MethodPost, tls.URL+"/mcp", strings.NewReader(waitRPC))
	req.Header.Set("Authorization", "Bearer valid-token")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := tls.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-observed:
	case <-time.After(time.Second):
		t.Fatal("observer survived disconnected client")
	}
	if backend.calledName != "" {
		t.Fatal("disconnect invoked another tool")
	}
}

// This proxy's connection enforces an idle read deadline, modeling the edge
// timeout independently of the longer origin/tool deadline. Both the old silent
// JSON response and an initial-only SSE response fail; periodic SSE survives.
type idleReadConn struct {
	net.Conn
	idle time.Duration
}

func (c idleReadConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.idle)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}
func TestMCPStreamSurvivesProxyIdleDeadline(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"silent JSON", "initial SSE only", "heartbeat SSE"} {
		t.Run(mode, func(t *testing.T) {
			// End the successful long poll only after the proxy has actually
			// received periodic frames. Wall time cannot prove that happened:
			// a loaded runner may pause either side past a short deadline.
			heartbeatsSeen := make(chan struct{})
			backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
				return func(ctx context.Context) (any, error) {
					if mode == "heartbeat SSE" {
						select {
						case <-heartbeatsSeen:
							return map[string]bool{"still_running": true}, nil
						case <-ctx.Done():
							return nil, ctx.Err()
						}
					}
					select {
					case <-time.After(350 * time.Millisecond):
						return map[string]bool{"still_running": true}, nil
					case <-ctx.Done():
						return nil, ctx.Err()
					}
				}, nil
			}}
			server := streamTestServer(t, backend)
			if mode == "initial SSE only" {
				server.mcpHeartbeatInterval = time.Second
			}
			handler := server.Handler()
			if mode == "silent JSON" {
				handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					select {
					case <-time.After(350 * time.Millisecond):
						handlerResult := `{"jsonrpc":"2.0","id":"exact-request","result":{}}`
						_, _ = io.WriteString(w, handlerResult)
					case <-r.Context().Done():
					}
				})
			}
			origin := httptest.NewTLSServer(handler)
			defer origin.Close()
			transport := origin.Client().Transport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				idle := 100 * time.Millisecond
				if mode == "heartbeat SSE" {
					// The frame handshake supplies the success condition. This
					// remains a deadline, but is only a failure backstop under load.
					idle = 5 * time.Second
				}
				return idleReadConn{Conn: conn, idle: idle}, nil
			}
			proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstream := r.Clone(r.Context())
				upstream.RequestURI = ""
				upstream.URL.Scheme = "https"
				upstream.URL.Host = strings.TrimPrefix(origin.URL, "https://")
				response, err := transport.RoundTrip(upstream)
				if err != nil {
					w.WriteHeader(524)
					return
				}
				defer response.Body.Close()
				w.Header().Set("Content-Type", response.Header.Get("Content-Type"))
				w.WriteHeader(response.StatusCode)
				buffer := make([]byte, 4096)
				var heartbeatBytes string
				heartbeatCount := 0
				for {
					n, err := response.Body.Read(buffer)
					if n > 0 {
						if mode == "heartbeat SSE" && heartbeatCount < 3 {
							heartbeatBytes += string(buffer[:n])
							heartbeatCount = strings.Count(heartbeatBytes, ": keepalive\n\n")
							if heartbeatCount >= 3 {
								close(heartbeatsSeen)
							}
						}
						if _, writeErr := w.Write(buffer[:n]); writeErr != nil {
							return
						}
						if flushErr := http.NewResponseController(w).Flush(); flushErr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}))
			defer proxy.Close()
			req, _ := http.NewRequest(http.MethodPost, proxy.URL+"/mcp", strings.NewReader(waitRPC))
			if mode == "heartbeat SSE" {
				ctx, cancel := context.WithTimeout(req.Context(), 10*time.Second)
				defer cancel()
				req = req.WithContext(ctx)
			}
			req.Header.Set("Authorization", "Bearer valid-token")
			req.Header.Set("Accept", "text/event-stream")
			response, err := proxy.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			complete := strings.Contains(string(body), "event: message\n")
			if complete != (mode == "heartbeat SSE") {
				t.Fatalf("mode %s status%d complete%v body%s", mode, response.StatusCode, complete, body)
			}
			if mode == "silent JSON" && response.StatusCode != 524 {
				t.Fatalf("silent response status%d, want524", response.StatusCode)
			}
		})
	}
}

func TestMCPStreamRequiresWriteDeadlineSupport(t *testing.T) {
	t.Parallel()
	backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
		t.Fatal("prepared stream without bounded writes")
		return nil, nil
	}}
	server := streamTestServer(t, backend)
	// Recorder flushes, but cannot enforce write deadlines. JSON compatibility
	// must remain available; advertising Flusher alone is not sufficient.
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, streamRequest(waitRPC, "text/event-stream", "valid-token"))
	if backend.calledName != "wait" || strings.Contains(recorder.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatal("unsupported deadline did not retain JSON")
	}
}

type shortFrameWriter struct{ streamFailureWriter }

func (w *shortFrameWriter) WriteString(s string) (int, error) { return len(s) - 1, nil }
func TestMCPStreamRejectsShortFrame(t *testing.T) {
	t.Parallel()
	writer := &shortFrameWriter{streamFailureWriter: streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}}
	deadline := time.Now().Add(time.Minute)
	if err := writeMCPFrame(writer, ": keepalive\n\n", deadline, time.Second); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short frame: %v", err)
	}
	if !writer.deadlines[len(writer.deadlines)-1].Equal(deadline) {
		t.Fatal("short write did not restore deadline")
	}
}

// net.Pipe forces real socket backpressure without relying on filling an OS
// send buffer. The initial or next heartbeat write must time out and join.
type blockedFrameWriter struct {
	conn   net.Conn
	header http.Header
}

func (w *blockedFrameWriter) Header() http.Header                { return w.header }
func (w *blockedFrameWriter) WriteHeader(int)                    {}
func (w *blockedFrameWriter) Write(p []byte) (int, error)        { return w.conn.Write(p) }
func (w *blockedFrameWriter) FlushError() error                  { return nil }
func (w *blockedFrameWriter) SetWriteDeadline(d time.Time) error { return w.conn.SetWriteDeadline(d) }
func TestMCPStreamBoundsSocketBackpressure(t *testing.T) {
	t.Parallel()
	for _, initial := range []bool{true, false} {
		t.Run(map[bool]string{true: "initial", false: "heartbeat"}[initial], func(t *testing.T) {
			writerConn, peer := net.Pipe()
			defer writerConn.Close()
			defer peer.Close()
			readerDone := make(chan struct{})
			if !initial {
				go func() {
					defer close(readerDone)
					b := make([]byte, len(": keepalive\n\n"))
					_, _ = io.ReadFull(peer, b)
				}()
			} else {
				close(readerDone)
			}
			writer := &blockedFrameWriter{conn: writerConn, header: make(http.Header)}
			server := &Server{mcpHeartbeatInterval: time.Millisecond, mcpStreamWriteTimeout: 30 * time.Millisecond}
			called := false
			started := time.Now()
			err := server.streamLongPoll(writer, httptest.NewRequest(http.MethodPost, "/mcp", nil), rpcResponse{}, func(ctx context.Context) (any, error) { called = true; <-ctx.Done(); return nil, ctx.Err() }, time.Now().Add(time.Minute))
			var netErr net.Error
			if !errors.As(err, &netErr) || !netErr.Timeout() {
				t.Fatalf("expected bounded socket timeout: %v", err)
			}
			if called == initial {
				t.Fatalf("observer called=%v initial=%v", called, initial)
			}
			if time.Since(started) > time.Second {
				t.Fatal("blocked stream exceeded write bound")
			}
			<-readerDone
		})
	}
}

func TestMCPStreamFinalToolErrorIsClassified(t *testing.T) {
	t.Parallel()
	backend := &streamingBackend{fakeBackend: &fakeBackend{}, prepare: func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) {
		return func(context.Context) (any, error) { return nil, errors.New("private token-canary") }, nil
	}}
	server := streamTestServer(t, backend)
	writer := &streamFailureWriter{ResponseRecorder: httptest.NewRecorder()}
	server.Handler().ServeHTTP(writer, streamRequest(waitRPC, "text/event-stream", "valid-token"))
	body := writer.Body.String()
	if strings.Contains(body, "token-canary") || !strings.Contains(body, "invalid observation") || strings.Count(body, "event: message\n") != 1 {
		t.Fatalf("unsafe/missing classified error: %s", body)
	}
}
