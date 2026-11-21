package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

type heldEventBody struct {
	ctx                context.Context
	cancelled, release chan struct{}
}

func (r heldEventBody) Read([]byte) (int, error) {
	<-r.ctx.Done()
	close(r.cancelled)
	<-r.release
	return 0, r.ctx.Err()
}
func TestBridgeEOFJoinsPushedStream(t *testing.T) {
	opened, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	defer close(release)
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return handlerTransport(h).RoundTrip(r)
		}
		close(opened)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(heldEventBody{r.Context(), cancelled, release})}, nil
	}))
	var diagnostic bytes.Buffer
	cfg.Diagnostic = &diagnostic
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send(nil, "notifications/initialized", nil)
	awaitEventSignal(t, opened)
	_ = p.input.Close()
	awaitEventSignal(t, cancelled)
	select {
	case err := <-p.done:
		t.Fatalf("bridge returned before stream joined: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// Allow the SSE reader to finish before waiting for Bridge itself.
	release <- struct{}{}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not join pushed stream")
	}
	if diagnostic.Len() != 0 {
		t.Fatal("clean EOF emitted a failure", diagnostic.String())
	}
}
func awaitEventSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(2 * time.Second):
		t.Fatal("event stream barrier timed out")
	}
}

func TestBridgePushRefusalDoesNotRetry(t *testing.T) {
	for _, response := range []struct {
		status     int
		kind, body string
	}{
		{403, "application/json", `{}`}, {200, "application/json", `{}`}, {200, "text/event-stream", "data: invalid\n\n"},
	} {
		t.Run(response.kind+response.body, func(t *testing.T) {
			h := server(t, fake.Bindings())
			calls := make(chan struct{}, 2)
			cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					return handlerTransport(h).RoundTrip(r)
				}
				calls <- struct{}{}
				return &http.Response{StatusCode: response.status, Header: http.Header{"Content-Type": {response.kind}}, Body: io.NopCloser(strings.NewReader(response.body))}, nil
			}))
			diagnosed := make(chan struct{}, 1)
			cfg.Diagnostic = eventDiagnostic{diagnosed}
			cfg.Config.Wait = func(context.Context, time.Duration) error { t.Error("permanent stream error retried"); return io.EOF }
			p := newBridgePeer(t, cfg)
			p.initialize()
			p.send(nil, "notifications/initialized", nil)
			awaitEventSignal(t, diagnosed)
			if len(calls) != 1 {
				t.Fatal("permanent stream error repeated")
			}
			p.tool("read", "repo_list", map[string]any{}, nil)
			if !bytes.Contains(p.frame(), []byte(`"id":"read"`)) {
				t.Fatal("push refusal blocked events_wait/tool RPCs")
			}
		})
	}
}

type eventDiagnostic struct{ written chan struct{} }

func (d eventDiagnostic) Write(p []byte) (int, error) {
	select {
	case d.written <- struct{}{}:
	default:
	}
	return len(p), nil
}

func awaitEventContext(t *testing.T, opened <-chan context.Context) context.Context {
	t.Helper()
	select {
	case ctx := <-opened:
		return ctx
	case <-time.After(2 * time.Second):
		t.Fatal("pushed stream did not open")
	}
	return nil
}
