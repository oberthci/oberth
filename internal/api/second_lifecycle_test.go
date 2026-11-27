package api_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSecondDrainDuringPreparation(t *testing.T) {
	for _, kind := range []string{"JSON wait", "SSE wait", "native log"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				var child context.Context
				f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
					secondPreparationBlock(c, b, kind, func(ctx context.Context) { child = ctx; close(entered); <-ctx.Done() })
				})
				assertSuccess(t, discover(f))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r := secondObservationRequest(ctx, kind)
				w := newStreamWriter()
				done := make(chan struct{})
				go func() { defer close(done); f.server.Handler().ServeHTTP(w, modernRequest(r)) }()
				<-entered
				f.server.BeginDrain()
				synctest.Wait()
				select {
				case <-child.Done():
				default:
					t.Error("BeginDrain did not cancel observation preparation")
				}
				cancel()
				<-done
				if f.clock.ActiveTickers() != 0 {
					t.Error("ticker remains")
				}
			})
		})
	}
}

func TestSecondBackupDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		opened := make(chan struct{})
		reader, writer := io.Pipe()
		defer func() { _ = writer.Close() }()
		f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
			c.Streams = &fake.Streams{Archive: func(context.Context, apicontract.Caller, apicontract.Empty) (io.ReadCloser, error) {
				close(opened)
				return reader, nil
			}}
		})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		r := httptest.NewRequest("POST", "/api/admin/database-backup", strings.NewReader("{}")).WithContext(ctx)
		r.Header.Set("Authorization", "Bearer valid")
		r.Header.Set("Content-Type", "application/json")
		done := make(chan struct{})
		go func() { defer close(done); f.server.Handler().ServeHTTP(newStreamWriter(), r) }()
		<-opened
		f.server.BeginDrain()
		synctest.Wait()
		select {
		case <-done:
		default:
			t.Error("idle backup stays blocked after drain")
		}
		cancel()
		<-done
	})
}

func secondPreparationBlock(c *api.Config, b *apicontract.Bindings, kind string, block func(context.Context)) {

	switch kind {
	case "JSON wait", "SSE wait":
		b.RunWait.Handler = apicontract.HandlerFunc[apicontract.RunWaitArgs, apicontract.WaitResult](func(ctx context.Context, _ apicontract.Caller, _ apicontract.RunWaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
			block(ctx)
			return nil, ctx.Err()
		})
	case "event stream":
		b.EventsWait.Handler = apicontract.HandlerFunc[apicontract.EventsWaitArgs, apicontract.EventResult](func(ctx context.Context, _ apicontract.Caller, _ apicontract.EventsWaitArgs) (apicontract.Call[apicontract.EventResult], error) {
			block(ctx)
			return nil, ctx.Err()
		})
	case "native log":
		c.Streams = &fake.Streams{Open: func(ctx context.Context, _ apicontract.Caller, _ apicontract.ContainerArgs) (io.ReadCloser, error) {
			block(ctx)
			return nil, ctx.Err()
		}}
	}
}

func secondObservationRequest(ctx context.Context, kind string) *http.Request {
	method, path, body := "POST", "/mcp", waitBody()
	if kind == "event stream" {
		method, body = "GET", ""
	}
	if kind == "native log" {
		method, path, body = "GET", "/api/runs/12345678/containerlog/stream?node_id=n&container=c", ""
	}
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Mcp-Session-Id", "00000000000000000000000000000001")
	if kind != "JSON wait" {
		r.Header.Set("Accept", "text/event-stream")
	}
	return r
}
