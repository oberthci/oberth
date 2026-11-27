package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

type streamWriter struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	fail      bool
}

func newStreamWriter() *streamWriter { return &streamWriter{ResponseRecorder: httptest.NewRecorder()} }
func (w *streamWriter) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	if w.fail {
		return errors.New("closed transport")
	}
	return nil
}
func (w *streamWriter) FlushError() error { w.Flush(); return nil }

type pendingRequest struct {
	cancel context.CancelFunc
	joined chan struct{}
	writer *streamWriter
}

func startSSE(f *fixture, path, method, body string) *pendingRequest {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequest(method, path, strings.NewReader(body)).WithContext(ctx)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Accept", "text/event-stream")
	r.Header.Set("Mcp-Session-Id", "00000000000000000000000000000001")
	w := newStreamWriter()
	joined := make(chan struct{})
	go func() { defer close(joined); f.server.Handler().ServeHTTP(w, modernRequest(r)) }()
	return &pendingRequest{cancel, joined, w}
}
func waitBody() string {
	return `{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"run_wait","arguments":{"id":"12345678"}}}`
}
func TestLongPoll64Accepts65RefusesAndReleases(t *testing.T) {
	started := make(chan struct{}, 64)
	release := make(chan struct{})
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
			started <- struct{}{}
			select {
			case <-release:
				return apicontract.WaitResult{}, nil
			case <-ctx.Done():
				return apicontract.WaitResult{}, ctx.Err()
			}
		}}
	})
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := request(f, "POST", "/mcp", []byte(waitBody()))
			if w.Code != 200 {
				t.Error(w.Code)
			}
		}()
	}
	for i := 0; i < 64; i++ {
		<-started
	}
	over := make(chan *httptest.ResponseRecorder, 1)
	go func() { over <- request(f, "POST", "/mcp", []byte(waitBody())) }()
	accepted := false
	select {
	case w := <-over:
		if !strings.Contains(w.Body.String(), "long-poll limit") {
			accepted = true
		}
	case <-started:
		accepted = true
	}
	inputs, _, _ := f.bindings.RunWait.Handler.(*fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]).Observed()
	close(release)
	wg.Wait()
	if len(inputs) != 64 {
		t.Fatal("saturated observation reached service preparation")
	}
	if accepted {
		t.Fatal("65th observation reached its handler")
	}
	assertSuccess(t, request(f, "POST", "/mcp", []byte(waitBody())))
}
func TestSSEPreparesBeforeStreamingAndHasOneFinalReply(t *testing.T) {
	f := setup(t, nil)
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody()))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Accept", "text/event-stream")
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if !strings.HasPrefix(w.Body.String(), ": keepalive\n\n") || strings.Count(w.Body.String(), "event: message") != 1 || len(w.deadlines) != 6 {
		t.Fatal(w.Body.String())
	}
	for _, deadline := range w.deadlines[2:] {
		if !deadline.IsZero() && deadline.Sub(f.clock.Now()) != 5*time.Second {
			t.Fatal("frame write deadline changed")
		}
	}
	if f.clock.ActiveTickers() != 0 {
		t.Fatal("heartbeat did not join")
	}
	f = setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{PrepareError: publicerr.New(publicerr.Forbidden, "Run is unavailable.", "Use a visible run.", nil)}
	})
	w = newStreamWriter()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if strings.HasPrefix(w.Body.String(), ":") || w.Header().Get("Content-Type") == "text/event-stream" {
		t.Fatal("started streaming before authorization")
	}
}
func TestStreamCancellationJoinsWriterAndDrainReturnsRestartCode(t *testing.T) {
	for _, drain := range []bool{false, true} {
		t.Run(fmt.Sprint(drain), func(t *testing.T) {
			started := make(chan struct{})
			f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
				b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
					close(started)
					<-ctx.Done()
					return apicontract.WaitResult{}, ctx.Err()
				}}
			})
			p := startSSE(f, "/mcp", "POST", waitBody())
			<-started
			if drain {
				f.server.BeginDrain()
			} else {
				p.cancel()
			}
			<-p.joined
			p.cancel()
			if f.clock.ActiveTickers() != 0 {
				t.Fatal("writer outlived request")
			}
			if drain && !strings.Contains(p.writer.Body.String(), `"code":-32001`) {
				t.Fatal(p.writer.Body.String())
			}
		})
	}
}
func TestSSEAcceptRequiresExplicitNonzeroQuality(t *testing.T) {
	for _, accept := range []string{"", "*/*", "text/event-stream;q=0", "text/event-stream;q=0.5,text/event-stream;q=0"} {
		f := setup(t, nil)
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody()))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		r.Header.Set("Accept", accept)
		w := newStreamWriter()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		if strings.Contains(w.Header().Get("Content-Type"), "event-stream") {
			t.Fatal("implicit SSE accepted")
		}
	}
}

// Covers: REQ-C1-002
func TestPrivateBoundedDiagnosticsContract(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Error: errors.New("token=private-key secret=private-value")}
	})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody()))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Accept", "text/event-stream")
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	data, err := json.Marshal(f.diagnostics.Items())
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"private-key", "private-value", "private-id", "12345678", "operator", "request-1", "Authorization"} {
		if strings.Contains(string(data)+w.Body.String(), private) {
			t.Fatalf("diagnostic disclosed %s", private)
		}
	}
	items := f.diagnostics.Items()
	if len(items) != 1 || len(items[0].Reference) != 32 || items[0].Tool != "run_wait" || items[0].Frames != 2 || !strings.Contains(w.Body.String(), "Reference: "+items[0].Reference) {
		t.Fatal(string(data))
	}
}
func TestNative64Accepts65RefusesAndCancellationClosesSource(t *testing.T) {
	opened := make(chan struct{}, 64)
	closed := make(chan struct{}, 65)
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.Streams = &fake.Streams{Open: func(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error) {
			reader, writer := io.Pipe()
			opened <- struct{}{}
			return &closeReader{ReadCloser: reader, writer: writer, closed: closed}, nil
		}}
	})
	pending := []*pendingRequest{}
	for i := 0; i < 64; i++ {
		c := admin()
		c.CredentialID = fmt.Sprint(i)
		f.auth.Set(c, nil)
		pending = append(pending, startSSE(f, "/api/runs/12345678/containerlog/stream?node_id=node&container=main", "GET", ""))
		<-opened
	}
	over := startSSE(f, "/api/runs/12345678/containerlog/stream?node_id=node&container=main", "GET", "")
	accepted := false
	select {
	case <-over.joined:
		accepted = over.writer.Code != 429
	case <-opened:
		accepted = true
	}
	over.cancel()
	<-over.joined
	if accepted {
		for _, p := range pending {
			p.cancel()
		}
		for _, p := range pending {
			<-p.joined
		}
		t.Fatal("65th native stream reached its source")
	}

	for _, p := range pending {
		p.cancel()
	}
	for _, p := range pending {
		<-p.joined
	}
	for i := 0; i < 64; i++ {
		<-closed
	}
	if f.clock.ActiveTickers() != 0 {
		t.Fatal("native watcher did not join")
	}
}

type closeReader struct {
	io.ReadCloser
	writer *io.PipeWriter
	closed chan struct{}
	once   sync.Once
}

func (r *closeReader) Close() error {
	r.once.Do(func() { _ = r.ReadCloser.Close(); _ = r.writer.Close(); r.closed <- struct{}{} })
	return nil
}
func TestNativeAndBackupStreamsPreservePlainBytes(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct{ path, method, body, want string }{
		{"/api/runs/12345678/containerlog/stream?node_id=node&container=main", "GET", "", "native bytes\n"},
		{"/api/admin/database-backup", "POST", "{}", "archive bytes"},
	} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
		r.Header.Set("Authorization", "Bearer valid")
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Accept-Encoding", "gzip")
		w := newStreamWriter()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		if w.Body.String() != tc.want || w.Header().Get("Content-Encoding") != "" {
			t.Fatal(w.Body.String())
		}
	}
}

func TestStreamReauthenticationAt30SecondsClosesChangedAuthority(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
				close(started)
				<-ctx.Done()
				return apicontract.WaitResult{}, ctx.Err()
			}}
		})
		p := startSSE(f, "/mcp", "POST", waitBody())
		<-started
		for i := 0; i < 2; i++ {
			<-f.clock.Started
		}
		c := admin()
		c.Actor.Scope = []string{"other"}
		f.auth.Set(c, nil)
		f.clock.Advance(30 * time.Second)
		synctest.Wait()
		select {
		case <-p.joined:
		default:
			p.cancel()
			<-p.joined
			t.Fatal("revoked stream stayed open")
		}
		p.cancel()
		if f.clock.ActiveTickers() != 0 {
			t.Fatal("reauthorization did not join writer")
		}
		if !strings.Contains(p.writer.Body.String(), `"isError":true`) {
			t.Fatal("changed authority retained delivery")
		}
	})
}
func TestNativeRechecksTargetAndCredentials(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		closed := make(chan struct{}, 1)
		opened := make(chan struct{})
		f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
			c.Streams = &fake.Streams{Open: func(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error) {
				reader, writer := io.Pipe()
				close(opened)
				return &closeReader{ReadCloser: reader, writer: writer, closed: closed}, nil
			}}
		})
		p := startSSE(f, "/api/runs/12345678/containerlog/stream?node_id=node&container=main", "GET", "")
		<-opened
		<-f.clock.Started
		c := admin()
		c.Actor.Admin = false
		f.auth.Set(c, nil)
		f.clock.Advance(30 * time.Second)
		synctest.Wait()
		select {
		case <-p.joined:
		default:
			p.cancel()
			<-p.joined
			t.Fatal("revoked stream stayed open")
		}
		p.cancel()
		<-closed
		if f.clock.ActiveTickers() != 0 {
			t.Fatal("native authorization watcher leaked")
		}
	})
}
