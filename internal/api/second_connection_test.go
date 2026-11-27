package api_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

// The listener runs net/http over in-memory pipes, without opening any socket.
type secondListener struct {
	pending chan net.Conn
	done    chan struct{}
	once    sync.Once
}

func (l *secondListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.pending:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}
func (l *secondListener) Close() error   { l.once.Do(func() { close(l.done) }); return nil }
func (l *secondListener) Addr() net.Addr { return secondAddr("memory") }

type secondAddr string

func (a secondAddr) Network() string { return "pipe" }
func (a secondAddr) String() string  { return string(a) }
func (l *secondListener) dial(ctx context.Context, _, _ string) (net.Conn, error) {
	c, s := net.Pipe()
	select {
	case l.pending <- s:
		return c, nil
	case <-ctx.Done():
		_ = c.Close()
		_ = s.Close()
		return nil, ctx.Err()
	case <-l.done:
		_ = c.Close()
		_ = s.Close()
		return nil, net.ErrClosed
	}
}
func secondHTTP(t *testing.T, f *fixture) (*http.Client, *http.Server) {
	t.Helper()
	l := &secondListener{pending: make(chan net.Conn), done: make(chan struct{})}
	hs := &http.Server{Handler: f.server.Handler(), WriteTimeout: f.server.WriteTimeout()}
	done := make(chan struct{})
	go func() { defer close(done); _ = hs.Serve(l) }()
	tr := &http.Transport{DialContext: l.dial}
	cl := &http.Client{Transport: tr}
	t.Cleanup(func() { tr.CloseIdleConnections(); _ = hs.Close(); <-done })
	return cl, hs
}
func secondDo(cl *http.Client, path, body string) (*http.Response, error) {
	r, _ := http.NewRequest("POST", "http://memory"+path, strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	r = modernRequest(r)
	r.ContentLength = -1
	return cl.Do(r)
}

// testNow reads the virtual timer clock supplied to the server by this fixture.
func testNow() time.Time { timer := time.NewTimer(0); defer timer.Stop(); return <-timer.C }
func TestSecondSlowHandlerCannotHoldHTTPPastDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		expired, release := make(chan struct{}), make(chan struct{})
		f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
			c.MaximumWait = 25 * time.Millisecond
			c.Runtime.Now = testNow
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
				<-ctx.Done()
				close(expired)
				<-release
				return apicontract.WaitResult{}, nil
			}}
		})
		client, _ := secondHTTP(t, f)
		reply := make(chan string, 1)
		go func() {
			r, err := secondDo(client, "/mcp", waitBody())
			if err != nil {
				reply <- err.Error()
				return
			}
			defer func() { _ = r.Body.Close() }()
			data, err := io.ReadAll(r.Body)
			if err != nil {
				reply <- err.Error()
				return
			}
			reply <- string(data)
		}()
		<-expired
		synctest.Wait()
		var body string
		select {
		case body = <-reply:
		default:
			t.Error("expired handler retained caller connection")
		}
		close(release)
		if body == "" {
			body = <-reply
		}
		if !strings.Contains(body, `"timed_out":true`) {
			t.Fatal("ceiling did not return its timeout result", body)
		}
	})
}
func TestSecondTimeoutDeliveryModes(t *testing.T) {
	for _, sse := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
				c.MaximumWait = 20 * time.Millisecond
				b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
					<-ctx.Done()
					return apicontract.WaitResult{}, ctx.Err()
				}}
			})
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody()))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer valid")
			if sse {
				r.Header.Set("Accept", "text/event-stream")
			}
			w := newStreamWriter()
			f.server.Handler().ServeHTTP(w, modernRequest(r))
			if !strings.Contains(w.Body.String(), `"timed_out":true`) || sse && strings.Count(w.Body.String(), "event: message") != 1 {
				t.Fatal("timeout delivery lost its one final result", w.Body.String())
			}
		})
	}
}
