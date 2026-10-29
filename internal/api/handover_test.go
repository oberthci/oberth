package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestDrainReadinessAndNewMutations(t *testing.T) {
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	server.BeginDrain()
	server.BeginDrain()
	for _, path := range []string{"/readyz", "/healthz", "/mcp"} {
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if path == "/mcp" {
			req = streamRequest(waitRPC, "application/json", "valid-token")
		}
		server.Handler().ServeHTTP(recorder, req)
		want := http.StatusServiceUnavailable
		if path == "/healthz" {
			want = http.StatusOK
		}
		if recorder.Code != want {
			t.Fatalf("%s status=%d", path, recorder.Code)
		}
	}
	if backend.calledName != "" {
		t.Fatal("admitted tool while draining")
	}
}

func TestDrainLongPollReturnsReconnectAndJoins(t *testing.T) {
	for _, accept := range []string{"application/json", "text/event-stream"} {
		t.Run(accept, func(t *testing.T) {
			started, stopped := make(chan struct{}), make(chan struct{})
			backend := &handoverBackend{streamingBackend: &streamingBackend{fakeBackend: &fakeBackend{}}}
			observe := func(ctx context.Context) (any, error) {
				close(started)
				<-ctx.Done()
				close(stopped)
				return nil, ctx.Err()
			}
			backend.observe = observe
			backend.prepare = func(context.Context, Actor, string, json.RawMessage) (PreparedLongPoll, error) { return observe, nil }
			server, err := New(backend, backend, backend, "test")
			if err != nil {
				t.Fatal(err)
			}
			httpServer := httptest.NewServer(server.Handler())
			defer httpServer.Close()
			req, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(waitRPC))
			req.Header.Set("Authorization", "Bearer valid-token")
			req.Header.Set("Accept", accept)
			result := make(chan string, 1)
			go func() {
				response, err := httpServer.Client().Do(req)
				if err != nil {
					result <- err.Error()
					return
				}
				defer response.Body.Close()
				body, _ := io.ReadAll(response.Body)
				result <- string(body)
			}()
			select {
			case <-started:
			case <-time.After(testtimeout.Start(time.Second)):
				t.Fatal("observer never started")
			}
			server.BeginDrain()
			shutdownCtx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(time.Second))
			defer cancel()
			if err := httpServer.Config.Shutdown(shutdownCtx); err != nil {
				t.Fatal(err)
			}
			select {
			case body := <-result:
				if !strings.Contains(body, `"code":-32001`) || strings.Contains(body, `"isError"`) {
					t.Fatalf("lost reconnect response: %s", body)
				}
			case <-shutdownCtx.Done():
				t.Fatal("stream did not drain")
			}
			select {
			case <-stopped:
			default:
				t.Fatal("observer outlived drain")
			}
		})
	}
}

type handoverBackend struct {
	*streamingBackend
	observe func(context.Context) (any, error)
}

func (backend *handoverBackend) CallTool(ctx context.Context, _ Actor, _ string, _ json.RawMessage) (any, error) {
	return backend.observe(ctx)
}

func TestDrainLetsAcceptedMutationFinish(t *testing.T) {
	started, finish := make(chan struct{}), make(chan struct{})
	backend := &handoverBackend{streamingBackend: &streamingBackend{fakeBackend: &fakeBackend{}}, observe: func(ctx context.Context) (any, error) {
		close(started)
		select {
		case <-ctx.Done():
			t.Error("drain canceled accepted mutation")
			return nil, ctx.Err()
		case <-finish:
			return map[string]bool{"ok": true}, nil
		}
	}}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()
	request, _ := http.NewRequest(http.MethodPost, httpServer.URL+"/mcp", strings.NewReader(strings.Replace(waitRPC, `"wait"`, `"promote"`, 1)))
	request.Header.Set("Authorization", "Bearer valid-token")
	done := make(chan error, 1)
	go func() {
		response, err := httpServer.Client().Do(request)
		if err == nil {
			_, err = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("mutation not started")
	}
	server.BeginDrain()
	select {
	case <-done:
		t.Fatal("mutation aborted during drain")
	default:
	}
	close(finish)
	ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Start(time.Second))
	defer cancel()
	if err := httpServer.Config.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

type blockedReadyBackend struct {
	*fakeBackend
	entered, release chan struct{}
}

func (backend *blockedReadyBackend) Ready(context.Context) error {
	close(backend.entered)
	<-backend.release
	return nil
}

func TestReadinessCannotReturnGreenAfterDrain(t *testing.T) {
	backend := &blockedReadyBackend{fakeBackend: &fakeBackend{}, entered: make(chan struct{}), release: make(chan struct{})}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		close(done)
	}()
	<-backend.entered
	server.BeginDrain()
	close(backend.release)
	<-done
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("in-flight ready probe returned %d", response.Code)
	}
}

func TestDrainConvertsCanceledReadsAndAuthToUnavailable(t *testing.T) {
	for _, phase := range []string{"authentication", "status"} {
		t.Run(phase, func(t *testing.T) {
			backend := &drainingReadBackend{fakeBackend: &fakeBackend{}, phase: phase, started: make(chan struct{})}
			server, err := New(backend, backend, backend, "test")
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			request.Header.Set("Authorization", "Bearer valid-token")
			done := make(chan struct{})
			go func() { defer close(done); server.Handler().ServeHTTP(recorder, request) }()
			<-backend.started
			server.BeginDrain()
			select {
			case <-done:
			case <-time.After(testtimeout.Start(time.Second)):
				t.Fatal("read did not stop")
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("canceled %s returned %d", phase, recorder.Code)
			}
		})
	}
}

type drainingReadBackend struct {
	*fakeBackend
	phase   string
	started chan struct{}
}

func (backend *drainingReadBackend) Authenticate(ctx context.Context, token string) (Actor, error) {
	if backend.phase == "authentication" {
		close(backend.started)
		<-ctx.Done()
		return Actor{}, ctx.Err()
	}
	return backend.fakeBackend.Authenticate(ctx, token)
}
func (backend *drainingReadBackend) Status(ctx context.Context, _ Actor) (any, error) {
	close(backend.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestDrainDoesNotAdmitMutationWhoseBodyArrivesLate(t *testing.T) {
	backend := &fakeBackend{}
	server, err := New(backend, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	reader, writer := io.Pipe()
	defer reader.Close()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/mcp", reader)
	request.Header.Set("Authorization", "Bearer valid-token")
	done := make(chan struct{})
	go func() { defer close(done); server.Handler().ServeHTTP(recorder, request) }()
	body := strings.Replace(waitRPC, `"wait"`, `"promote"`, 1)
	if _, err := io.WriteString(writer, body[:len(body)/2]); err != nil {
		t.Fatal(err)
	}
	server.BeginDrain()
	if _, err := io.WriteString(writer, body[len(body)/2:]); err != nil {
		t.Fatal(err)
	}
	writer.Close()
	select {
	case <-done:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("late body did not finish")
	}
	if recorder.Code != http.StatusServiceUnavailable || backend.calledName != "" {
		t.Fatalf("late mutation admitted: status=%d name=%s", recorder.Code, backend.calledName)
	}
}
