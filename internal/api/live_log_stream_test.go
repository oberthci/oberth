package api

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"
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
	httpServer.Config.WriteTimeout = 50 * time.Millisecond
	httpServer.Start()
	defer httpServer.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
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
	time.Sleep(100 * time.Millisecond)
	close(release)
	tail, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tail), `"done":true`) {
		t.Fatal(string(tail))
	}
}
