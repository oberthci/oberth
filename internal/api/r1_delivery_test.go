package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestUnknownCauseHasOnePrivateStructuredLogAndSafeDiagnostic(t *testing.T) {
	for _, sse := range []bool{false, true} {
		var logs bytes.Buffer
		f := setup(t, func(c *api.Config, b *apicontract.Bindings) {
			c.FailureLog = slog.New(slog.NewJSONHandler(&logs, nil))
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Error: errors.New("private-secret-cause")}
		})
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody()))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		if sse {
			r.Header.Set("Accept", "text/event-stream")
		}
		w := newStreamWriter()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		items := f.diagnostics.Items()
		if len(items) != 1 {
			t.Fatal(items)
		}
		diagnostic, _ := json.Marshal(items[0])
		if strings.Contains(string(diagnostic), "private-secret-cause") || strings.Contains(w.Body.String(), "private-secret-cause") {
			t.Fatal("dependency error escaped into bounded channel")
		}
		lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
		if len(lines) != 1 {
			t.Fatal("unknown cause logged more than once", logs.String())
		}
		var entry map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
			t.Fatal(err)
		}
		if entry["reference"] != items[0].Reference || entry["cause"] != "private-secret-cause" || !strings.Contains(w.Body.String(), "Reference: "+items[0].Reference) {
			t.Fatal("private cause cannot be correlated", entry)
		}
	}
}
func TestJSONWaitDrainHasRestartCode(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
				close(started)
				<-ctx.Done()
				return apicontract.WaitResult{}, ctx.Err()
			}}
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(waitBody())).WithContext(ctx)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		done := make(chan *httptest.ResponseRecorder, 1)
		go func() { w := httptest.NewRecorder(); f.server.Handler().ServeHTTP(w, modernRequest(r)); done <- w }()
		<-started
		f.server.BeginDrain()
		synctest.Wait()
		var w *httptest.ResponseRecorder
		select {
		case w = <-done:
		default:
			cancel()
			<-done
			t.Fatal("drain retained a JSON wait")
		}
		if !strings.Contains(w.Body.String(), `"code":-32001`) {
			t.Fatal("JSON observation lost restart refusal", w.Body.String())
		}
	})
}

func TestNativeTargetRevocationClosesBlockedRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started := make(chan struct{})
		closed := make(chan struct{}, 1)
		f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
			c.Streams = &fake.Streams{Open: func(context.Context, apicontract.Caller, apicontract.ContainerArgs) (io.ReadCloser, error) {
				r, w := io.Pipe()
				close(started)
				return &closeReader{ReadCloser: r, writer: w, closed: closed}, nil
			}, Check: func(context.Context, apicontract.Caller, string) error {
				return publicerr.New(publicerr.Forbidden, "Run is unavailable.", "Use a visible run.", nil)
			}}
		})
		p := startSSE(f, "/api/runs/12345678/containerlog/stream?node_id=node&container=main", "GET", "")
		<-started
		<-f.clock.Started
		f.clock.Advance(time.Second)
		synctest.Wait()
		select {
		case <-p.joined:
		default:
			p.cancel()
			<-p.joined
			t.Fatal("target revocation retained native stream")
		}
		p.cancel()
	})
}
func TestHeartbeat20AndNativeWrite30SecondBudgets(t *testing.T) {
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
		<-f.clock.Started
		<-f.clock.Started
		f.clock.Advance(20 * time.Second)
		synctest.Wait()
		p.cancel()
		<-p.joined
		if strings.Count(p.writer.Body.String(), ": keepalive") != 2 {
			t.Fatal("heartbeat boundary", p.writer.Body.String())
		}
	})
	f := setup(t, nil)
	r := httptest.NewRequest("POST", "/api/admin/database-backup", strings.NewReader(`{}`))
	r.Header.Set("Authorization", "Bearer valid")
	r.Header.Set("Content-Type", "application/json")
	w := newStreamWriter()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if len(w.deadlines) != 4 || !w.deadlines[1].IsZero() || w.deadlines[2].Sub(f.clock.Now()) != 30*time.Second || !w.deadlines[3].IsZero() {
		t.Fatal("native write deadline", w.deadlines)
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("private read failure") }
func (brokenBody) Close() error             { return nil }
func TestBodyReadFailureAndOversizeHaveDistinctTruthfulReplies(t *testing.T) {
	f := setup(t, nil)
	r := httptest.NewRequest("POST", "/mcp", nil)
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	r.Body = brokenBody{}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "could not be read completely") || strings.Contains(w.Body.String(), "private read failure") {
		t.Fatal(w.Body.String())
	}
	w = request(f, "POST", "/mcp", bytes.Repeat([]byte(" "), apicontract.RequestBytes+1))
	if w.Code != 413 || !strings.Contains(w.Body.String(), "exceeds this route") {
		t.Fatal(w.Body.String())
	}
}
func TestRPCObjectAndBooleanIDsRefused(t *testing.T) {
	f := setup(t, nil)
	for _, id := range []string{`{}`, `[]`, `true`} {
		w := request(f, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":`+id+`,"method":"ping"}`))
		if !strings.Contains(w.Body.String(), `"code":-32600`) {
			t.Fatal("invalid RPC ID accepted", w.Body.String())
		}
	}
}
func TestGetMCPIsRemoved(t *testing.T) {
	f := setup(t, nil)
	w := request(f, "GET", "/mcp", nil)
	if w.Code != 405 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestEnrollmentDeadlineIs15Seconds(t *testing.T) {
	var deadline time.Time
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.Enroll = apicontract.HandlerFunc[model.InviteRedemption, model.InviteCertificate](func(ctx context.Context, _ apicontract.Caller, _ model.InviteRedemption) (apicontract.Call[model.InviteCertificate], error) {
			deadline, _ = ctx.Deadline()
			return func(context.Context) (model.InviteCertificate, error) { return model.InviteCertificate{}, nil }, nil
		})
	})
	parent, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	outer, _ := parent.Deadline()
	r := httptest.NewRequest("POST", "/mcp/enroll", strings.NewReader(redemptionBody())).WithContext(parent)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	assertSuccess(t, w)
	if d := outer.Sub(deadline); d < 14*time.Second || d > 16*time.Second {
		t.Fatal("enrollment deadline", d)
	}
}
