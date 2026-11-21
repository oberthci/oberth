package api_test

import (
	"context"
	"fmt"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestSecondConcurrentResourceBound(t *testing.T) {
	const n = 96
	entered := make(chan struct{}, n)
	release := make(chan struct{})
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Run: func(context.Context) (apicontract.RunRecord, error) {
			entered <- struct{}{}
			<-release
			return apicontract.RunRecord{}, nil
		}}
	})
	assertSuccess(t, initialize(f))
	var successes atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := request(f, "POST", "/mcp", []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"resources/subscribe","params":{"uri":"run://run-%d"}}`, i)))
			if !strings.Contains(w.Body.String(), `"error":`) {
				successes.Add(1)
			}
		}(i)
	}
	for i := 0; i < n; i++ {
		<-entered
	}
	close(release)
	wg.Wait()
	t.Logf("parallel subscriptions accepted=%d limit=64", successes.Load())
	if successes.Load() != 64 {
		t.Errorf("resource cap exceeded: %d", successes.Load())
	}
}

func TestSecondDrainDuringPreparation(t *testing.T) {
	for _, kind := range []string{"JSON wait", "SSE wait", "event stream", "native log"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				var child context.Context
				f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
					secondPreparationBlock(c, b, kind, func(ctx context.Context) { child = ctx; close(entered); <-ctx.Done() })
				})
				assertSuccess(t, initialize(f))
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				r := secondObservationRequest(ctx, kind)
				w := newStreamWriter()
				done := make(chan struct{})
				go func() { defer close(done); f.server.Handler().ServeHTTP(w, r) }()
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
