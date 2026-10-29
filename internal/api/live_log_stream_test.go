package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

type liveStreamBackend struct {
	fakeBackend
	read func(context.Context, Actor, string, int64) (any, error)
}

func (backend *liveStreamBackend) RunLogLive(ctx context.Context, actor Actor, id string, offset int64) (any, error) {
	return backend.read(ctx, actor, id, offset)
}
func liveStreamRequest(ctx context.Context, query string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/livelog/stream"+query, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer valid-token")
	return request
}
func TestLiveLogStreamTailAndCompletion(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		offsets := []int64{-1, 5, 5, 10}
		chunks := []LiveLogResponse{{Chunk: "one\n", Offset: 5}, {Offset: 5}, {Chunk: "two\n", Offset: 10}, {Offset: 10, Done: true}}
		calls := 0
		backend := &liveStreamBackend{read: func(_ context.Context, actor Actor, id string, offset int64) (any, error) {
			if calls >= len(chunks) {
				t.Fatal("read after done")
			}
			if offset != offsets[calls] || actor.Identity != "agent@host" || id != "run-1" {
				t.Fatalf("unexpected read: %s %s %d", actor.Identity, id, offset)
			}
			chunk := chunks[calls]
			calls++
			return chunk, nil
		}}
		server, err := New(backend, backend, backend, "test")
		if err != nil {
			t.Fatal(err)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, liveStreamRequest(context.Background(), ""))
		body := response.Body.String()
		if response.Header().Get("Content-Type") != "text/event-stream" || !response.Flushed {
			t.Fatal(response)
		}
		if calls != 4 || strings.Count(body, "event: chunk\n") != 3 || !strings.Contains(body, `"done":true`) || !strings.Contains(body, `"chunk":"one\n"`) || !strings.Contains(body, `"chunk":"two\n"`) {
			t.Fatal(body)
		}
	})
}
func TestLiveLogStreamHeartbeatAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backend := &liveStreamBackend{read: func(context.Context, Actor, string, int64) (any, error) { return LiveLogResponse{Offset: 0}, nil }}
		server, err := New(backend, backend, backend, "test")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		response := httptest.NewRecorder()
		finished := make(chan struct{})
		go func() { defer close(finished); server.Handler().ServeHTTP(response, liveStreamRequest(ctx, "")) }()
		time.Sleep(31 * time.Second)
		cancel()
		<-finished
		if got := strings.Count(response.Body.String(), ": ping\n\n"); got != 3 {
			t.Fatalf("heartbeats = %d", got)
		}
		if strings.Contains(response.Body.String(), "event: chunk") {
			t.Fatal("empty running ticks emitted chunks")
		}
	})
}
func TestLiveLogStreamRejectsBeforeStreaming(t *testing.T) {
	for _, test := range []struct {
		name, query string
		auth        bool
		err         error
		status      int
	}{
		{name: "auth", status: 401},
		{name: "negative", query: "?offset=-2", auth: true, status: 400},
		{name: "invalid", query: "?offset=no", auth: true, status: 400},
		{name: "purged or forbidden view", auth: true, err: errors.New("unavailable evidence"), status: 410},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := 0
			backend := &liveStreamBackend{read: func(context.Context, Actor, string, int64) (any, error) { calls++; return nil, test.err }}
			server, err := New(backend, backend, backend, "test", WithErrorClassifier(func(error) (int, string) { return 410, "evidence unavailable" }))
			if err != nil {
				t.Fatal(err)
			}
			request := liveStreamRequest(context.Background(), test.query)
			if !test.auth {
				request.Header.Del("Authorization")
			}
			response := httptest.NewRecorder()
			server.Handler().ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Header().Get("Content-Type"), "event-stream") {
				t.Fatal(response)
			}
			if test.err == nil && calls != 0 {
				t.Fatal("invalid request reached view")
			}
		})
	}
}

// A real connection proves frames arrive before the handler completes and
// the server's ordinary WriteTimeout does not cut off the next frame.
func TestLiveLogStreamFlushAndWriteDeadline(t *testing.T) {
	release := make(chan struct{})
	backend := &liveStreamBackend{read: func(ctx context.Context, _ Actor, _ string, offset int64) (any, error) {
		if offset == -1 {
			return LiveLogResponse{Chunk: "first\n", Offset: 6}, nil
		}
		select {
		case <-release:
			return LiveLogResponse{Chunk: "last\n", Offset: 11, Done: true}, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewUnstartedServer(server.Handler())
	httpServer.Config.WriteTimeout = testtimeout.Scale(50 * time.Millisecond)
	httpServer.Start()
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(3*time.Second))
	defer cancel()
	request, _ := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+"/api/runs/run-1/livelog/stream", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(line, `"chunk":"first\n"`) {
			break
		}
	}
	time.Sleep(testtimeout.Scale(100 * time.Millisecond))
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tail), `"done":true`) {
		t.Fatal(string(tail))
	}
}

type idleLiveStreamBackend struct {
	liveStreamBackend
	size  atomic.Int64
	stats atomic.Int64
}

func (backend *idleLiveStreamBackend) LiveLogSize(id string) (int64, error) {
	if id != "canonical-run" {
		return 0, errors.New("unresolved log ID")
	}
	backend.stats.Add(1)
	return backend.size.Load(), nil
}

func TestLiveLogStreamIdleStatsAndTerminalRefresh(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		backend := &idleLiveStreamBackend{}
		var calls atomic.Int64
		var done atomic.Bool
		backend.read = func(_ context.Context, _ Actor, _ string, offset int64) (any, error) {
			calls.Add(1)
			chunk := ""
			if offset < backend.size.Load() {
				chunk = "appended"
			}
			return LiveLogResponse{RunID: "canonical-run", Offset: backend.size.Load(), Size: backend.size.Load(), Chunk: chunk, Done: done.Load()}, nil
		}
		server, err := New(backend, backend, backend, "test")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		finished := make(chan struct{})
		response := httptest.NewRecorder()
		go func() {
			defer close(finished)
			server.Handler().ServeHTTP(response, liveStreamRequest(ctx, "?offset=0"))
		}()
		synctest.Wait()
		time.Sleep(750 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 1 || backend.stats.Load() != 5 {
			t.Fatalf("idle reads=%d stats=%d", calls.Load(), backend.stats.Load())
		}
		backend.size.Store(8)
		time.Sleep(150 * time.Millisecond)
		synctest.Wait()
		if calls.Load() != 2 {
			t.Fatalf("append not read: %d", calls.Load())
		}
		done.Store(true)
		time.Sleep(1100 * time.Millisecond)
		synctest.Wait()
		select {
		case <-finished:
		default:
			t.Fatal("terminal state not refreshed")
		}
		if calls.Load() != 3 || !strings.Contains(response.Body.String(), `"done":true`) || strings.Count(response.Body.String(), `"chunk":"appended"`) != 1 {
			t.Fatalf("reads=%d response=%s", calls.Load(), response.Body.String())
		}
	})
}

type streamAuthFunc func(context.Context, string) (Actor, error)

func (f streamAuthFunc) Authenticate(ctx context.Context, token string) (Actor, error) {
	return f(ctx, token)
}

func TestLiveLogStreamRevalidatesCredentialAndAuthority(t *testing.T) {
	for _, outcome := range []string{"revoked", "rotated", "lookup failure", "empty identity", "scope changed", "admin changed", "fingerprint changed", "identity changed", "timeout"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reads, authCalls := 0, 0
				original := Actor{Identity: "agent@host", Fingerprint: "SHA256:key", Admin: true}
				auth := streamAuthFunc(func(ctx context.Context, token string) (Actor, error) {
					authCalls++
					if token != "valid-token" {
						t.Fatalf("wrong token: %q", token)
					}
					if authCalls == 1 {
						return original, nil
					}
					fresh := original
					switch outcome {
					case "revoked", "rotated", "lookup failure":
						return Actor{}, errors.New("private credential lookup detail")
					case "empty identity":
						fresh.Identity = ""
					case "scope changed":
						fresh.Scope = []string{"restricted"}
					case "admin changed":
						fresh.Admin = false
					case "fingerprint changed":
						fresh.Fingerprint = "SHA256:replacement"
					case "identity changed":
						fresh.Identity = "replacement"
					case "timeout":
						<-ctx.Done()
						return Actor{}, ctx.Err()
					}
					return fresh, nil
				})
				backend := &liveStreamBackend{read: func(context.Context, Actor, string, int64) (any, error) { reads++; return LiveLogResponse{}, nil }}
				server, err := New(auth, backend, backend, "test")
				if err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				start := time.Now()
				server.Handler().ServeHTTP(response, liveStreamRequest(t.Context(), ""))
				if elapsed := time.Since(start); elapsed < liveLogAuthInterval || elapsed > liveLogAuthInterval+5*time.Second+150*time.Millisecond {
					t.Fatalf("credential check unbounded: %s", elapsed)
				}
				body := response.Body.String()
				if authCalls != 2 || reads == 0 || strings.Count(body, "event: auth_error\n") != 1 || !strings.Contains(body, "reconnect with a current bearer token") || strings.Contains(body, "private credential") || strings.Contains(body, "event: chunk") {
					t.Fatalf("auth=%d reads=%d body=%s", authCalls, reads, body)
				}
			})
		})
	}
}

func TestLiveLogStreamValidCredentialRemainsOpen(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		backend := &liveStreamBackend{read: func(context.Context, Actor, string, int64) (any, error) { return LiveLogResponse{}, nil }}
		auth := streamAuthFunc(func(context.Context, string) (Actor, error) { calls++; return Actor{Identity: "agent@host"}, nil })
		server, err := New(auth, backend, backend, "test")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		finished := make(chan struct{})
		response := httptest.NewRecorder()
		go func() { defer close(finished); server.Handler().ServeHTTP(response, liveStreamRequest(ctx, "")) }()
		time.Sleep(61 * time.Second)
		cancel()
		<-finished
		if calls != 3 || strings.Contains(response.Body.String(), "event: auth_error") {
			t.Fatalf("auth=%d body=%s", calls, response.Body.String())
		}
	})
}

// Go-only CI counterpart to the browser suite's dropped-stream fallback and
// terminal-tail cases. Exercise real authenticated HTTP, not a frame mock.
func TestLiveLogStreamDroppedConnectionResumesExactCursor(t *testing.T) {
	for _, transport := range []string{"poll", "stream"} {
		t.Run(transport, func(t *testing.T) {
			first, tail := "first 🚀\n", "unterminated final 🚀"
			var resumed atomic.Bool
			backend := &liveStreamBackend{read: func(_ context.Context, actor Actor, id string, offset int64) (any, error) {
				if actor.Identity != "agent@host" || id != "run-1" {
					return nil, errors.New("unexpected authorization or run")
				}
				switch {
				case offset == -1:
					return LiveLogResponse{RunID: id, Chunk: first, Offset: int64(len(first)), Size: int64(len(first))}, nil
				case offset != int64(len(first)):
					return nil, errors.New("unexpected continuation cursor")
				case !resumed.Load():
					return LiveLogResponse{RunID: id, Offset: offset, Size: offset}, nil
				default:
					return LiveLogResponse{RunID: id, Chunk: tail, Offset: offset + int64(len(tail)), Size: offset + int64(len(tail)), Done: true}, nil
				}
			}}
			server, err := New(backend, backend, backend, "test")
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(3*time.Second))
			defer cancel()
			streamCtx, drop := context.WithCancel(ctx)
			defer drop()
			get := func(ctx context.Context, path string) *http.Response {
				t.Helper()
				request, err := http.NewRequestWithContext(ctx, http.MethodGet, httpServer.URL+path, nil)
				if err != nil {
					t.Fatal(err)
				}
				request.Header.Set("Authorization", "Bearer valid-token")
				response, err := http.DefaultClient.Do(request)
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != http.StatusOK {
					response.Body.Close()
					t.Fatalf("HTTP %d", response.StatusCode)
				}
				return response
			}
			readChunk := func(reader io.Reader) LiveLogResponse {
				t.Helper()
				scanner := bufio.NewScanner(reader)
				event := ""
				for scanner.Scan() {
					line := scanner.Text()
					if strings.HasPrefix(line, "event: ") {
						event = strings.TrimPrefix(line, "event: ")
					}
					if event == "chunk" && strings.HasPrefix(line, "data: ") {
						var chunk LiveLogResponse
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &chunk); err != nil {
							t.Fatal(err)
						}
						return chunk
					}
				}
				t.Fatalf("missing chunk: %v", scanner.Err())
				return LiveLogResponse{}
			}
			response := get(streamCtx, "/api/runs/run-1/livelog/stream")
			initial := readChunk(response.Body)
			drop()
			response.Body.Close()
			if initial.Chunk != first || initial.Done {
				t.Fatalf("initial: %+v", initial)
			}
			resumed.Store(true)
			path := fmt.Sprintf("/api/runs/run-1/livelog?offset=%d", initial.Offset)
			if transport == "stream" {
				path = fmt.Sprintf("/api/runs/run-1/livelog/stream?offset=%d", initial.Offset)
			}
			response = get(ctx, path)
			defer response.Body.Close()
			var final LiveLogResponse
			if transport == "poll" {
				if err := json.NewDecoder(response.Body).Decode(&final); err != nil {
					t.Fatal(err)
				}
			} else {
				final = readChunk(response.Body)
			}
			if final.Chunk != tail || !final.Done || final.Offset != int64(len(first)+len(tail)) || final.Offset != final.Size {
				t.Fatalf("resumed: %+v", final)
			}
		})
	}
}
