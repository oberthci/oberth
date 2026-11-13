package api

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

type containerStreamBackend struct {
	fakeBackend
	open   func(context.Context, Actor, string, string, string, bool) (io.ReadCloser, error)
	access func(context.Context, Actor, string) error
}

func (backend *containerStreamBackend) OpenRunContainerLog(ctx context.Context, actor Actor, id, node, container string, follow bool) (io.ReadCloser, error) {
	return backend.open(ctx, actor, id, node, container, follow)
}
func (backend *containerStreamBackend) CheckContainerLogAccess(ctx context.Context, actor Actor, id string) error {
	if backend.access != nil {
		return backend.access(ctx, actor, id)
	}
	return nil
}

type streamAuthFunc func(context.Context, string) (Actor, error)

func (auth streamAuthFunc) Authenticate(ctx context.Context, token string) (Actor, error) {
	return auth(ctx, token)
}
func containerStreamRequest(ctx context.Context, query string) *http.Request {
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/containerlog/stream?node_id=node&container=main"+query, nil).WithContext(ctx)
	request.Header.Set("Authorization", "Bearer valid-token")
	return request
}
func TestContainerLogHTTPPassesPlainBytes(t *testing.T) {
	body := "@@oberth-step-progress@@ " + `{"version":1,"burn":"test","step":"one","ordinal":0,"status":"queued"}` + "\n" + "::oberth-log:: plain\n[build/test] command\n"
	calls := 0
	backend := &containerStreamBackend{open: func(_ context.Context, actor Actor, id, node, container string, follow bool) (io.ReadCloser, error) {
		calls++
		if actor.Identity != "agent@host" || id != "run-1" || node != "node" || container != "main" || !follow {
			t.Fatal(actor, id, node, container, follow)
		}
		return io.NopCloser(strings.NewReader(body)), nil
	}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, containerStreamRequest(t.Context(), ""))
	if response.Code != 200 || !response.Flushed || response.Body.String() != body || calls != 1 || !strings.HasPrefix(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatal(response, calls)
	}
}

type countedContainerStream struct {
	io.Reader
	close func() error
}

func (stream countedContainerStream) Close() error { return stream.close() }
func TestContainerLogHTTPCancelJoinsIdleStream(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		reader, writer := io.Pipe()
		defer writer.Close()
		var closes atomic.Int32
		backend := &containerStreamBackend{open: func(context.Context, Actor, string, string, string, bool) (io.ReadCloser, error) {
			return countedContainerStream{Reader: reader, close: func() error { closes.Add(1); return reader.Close() }}, nil
		}}
		server, err := New(backend, backend, backend, "test")
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		joined := make(chan struct{})
		go func() {
			defer close(joined)
			server.Handler().ServeHTTP(httptest.NewRecorder(), containerStreamRequest(ctx, ""))
		}()
		synctest.Wait()
		cancel()
		<-joined
		if closes.Load() != 1 {
			t.Fatal("stream close count", closes.Load())
		}
	})
}
func TestContainerLogHTTPRechecksAuthority(t *testing.T) {
	for _, outcome := range []string{"revoked", "scope", "admin", "fingerprint", "identity", "timeout", "retention", "valid"} {
		t.Run(outcome, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				reader, writer := io.Pipe()
				defer writer.Close()
				calls := 0
				original := Actor{Identity: "agent@host", Fingerprint: "SHA256:key", Admin: true}
				auth := streamAuthFunc(func(ctx context.Context, _ string) (Actor, error) {
					calls++
					if calls == 1 {
						return original, nil
					}
					fresh := original
					switch outcome {
					case "revoked":
						return Actor{}, errors.New("private detail")
					case "scope":
						fresh.Scope = []string{"restricted"}
					case "admin":
						fresh.Admin = false
					case "fingerprint":
						fresh.Fingerprint = "replacement"
					case "identity":
						fresh.Identity = "replacement"
					case "timeout":
						<-ctx.Done()
						return Actor{}, ctx.Err()
					}
					return fresh, nil
				})
				backend := &containerStreamBackend{open: func(context.Context, Actor, string, string, string, bool) (io.ReadCloser, error) { return reader, nil }}
				if outcome == "retention" {
					backend.access = func(context.Context, Actor, string) error { return errors.New("evidence purged") }
				}
				server, err := New(auth, backend, backend, "test")
				if err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				joined := make(chan struct{})
				response := httptest.NewRecorder()
				go func() { defer close(joined); server.Handler().ServeHTTP(response, containerStreamRequest(ctx, "")) }()
				if outcome == "valid" {
					time.Sleep(61 * time.Second)
					cancel()
				}
				<-joined
				if outcome == "valid" && calls != 3 {
					t.Fatal("valid credential not refreshed", calls)
				}
				if response.Body.Len() != 0 {
					t.Fatal("protocol text injected", response.Body.String())
				}
			})
		})
	}
}
func TestContainerLogHTTPRejectsBeforeOpen(t *testing.T) {
	calls := 0
	backend := &containerStreamBackend{open: func(context.Context, Actor, string, string, string, bool) (io.ReadCloser, error) {
		calls++
		return nil, errors.New("unexpected")
	}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"node_id=&container=main", "node_id=node&follow=bad", "node_id=node&follow=true"} {
		request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/containerlog/stream?"+query, nil)
		request.Header.Set("Authorization", "Bearer valid-token")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != 400 {
			t.Fatal(query, response.Code)
		}
	}
	request := containerStreamRequest(t.Context(), "")
	request.Header.Del("Authorization")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 401 || calls != 0 {
		t.Fatal(response.Code, calls)
	}
}

func TestContainerLogHTTPAcceptsFullNativeNodeIDs(t *testing.T) {
	node := strings.Repeat("a", 253)
	backend := &containerStreamBackend{open: func(_ context.Context, _ Actor, _ string, actual, _ string, _ bool) (io.ReadCloser, error) {
		if actual != node {
			t.Fatal(actual)
		}
		return io.NopCloser(strings.NewReader("native output")), nil
	}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/api/runs/run-1/containerlog/stream?node_id="+node+"&container=main", nil)
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != 200 || response.Body.String() != "native output" {
		t.Fatal(response.Code, response.Body.String())
	}
}
