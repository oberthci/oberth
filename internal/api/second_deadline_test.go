package api

import (
	"context"
	"crypto/x509"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

type secondAuth struct{}

func (secondAuth) Bearer(context.Context, string) (apicontract.Caller, error) {
	return apicontract.Caller{CredentialID: "test", Actor: actor.Actor{Identity: "test", AuthMethod: "bearer"}}, nil
}
func (secondAuth) Certificate(context.Context, *x509.Certificate) (apicontract.Caller, error) {
	return apicontract.Caller{}, refusal("auth")
}

type secondDeadlineWriter struct {
	*httptest.ResponseRecorder
	writes  int
	flushed chan struct{}
	once    sync.Once
}

func (w *secondDeadlineWriter) SetWriteDeadline(time.Time) error { w.writes++; return nil }
func (w *secondDeadlineWriter) FlushError() error {
	w.Flush()
	w.once.Do(func() { close(w.flushed) })
	return nil
}
func TestSecondOrdinaryDeadline(t *testing.T) {
	for _, mutation := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			expired, release := make(chan struct{}), make(chan struct{})
			s := secondDeadlineServer(t, mutation, deadlineSignals{expired, release})
			r := httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"test","arguments":{}}}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer test")
			w := &secondDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), flushed: make(chan struct{})}
			done := make(chan struct{})
			go func() { defer close(done); s.Handler().ServeHTTP(w, modernRequest(r)) }()
			<-expired
			synctest.Wait()
			select {
			case <-w.flushed:
			default:
				close(release)
				<-done
				t.Fatal("deadline did not flush its reply")
			}
			select {
			case <-done:
				t.Error("request returned with service work unjoined")
			default:
			}
			reply := w.Body.String()
			if w.writes == 0 || strings.Contains(reply, "late-success") || !strings.Contains(reply, `"isError":true`) {
				t.Error("deadline did not produce a bounded refusal", reply)
			}
			if mutation && (!strings.Contains(reply, "may have completed") || !strings.Contains(reply, "run_get")) {
				t.Error("accepted action uncertainty lost", reply)
			}
			// The deliberately noncooperative fixture is released and joined. Go cannot
			// terminate another function; the caller already has its complete reply.
			close(release)
			<-done
			if w.Body.String() != reply {
				t.Fatal("late service result wrote a second reply")
			}
		})
	}
}

type deadlineSignals struct{ expired, release chan struct{} }

func secondDeadlineServer(t *testing.T, mutation bool, signals deadlineSignals) *Server {
	t.Helper()
	h := apicontract.HandlerFunc[apicontract.Empty, apicontract.Receipt](func(context.Context, apicontract.Caller, apicontract.Empty) (apicontract.Call[apicontract.Receipt], error) {
		return func(ctx context.Context) (apicontract.Receipt, error) {
			<-ctx.Done()
			close(signals.expired)
			<-signals.release
			return apicontract.Receipt{ID: "late-success"}, nil
		}, nil
	})
	reg, err := NewRegistry([]Tool{bindDefinition(apicontract.Binding[apicontract.Empty, apicontract.Receipt]{Handler: h}.Definition(apicontract.Metadata{Name: "test", Description: "Inspect a test operation.", Access: apicontract.Read, InputBytes: apicontract.RequestBytes, OutputBytes: apicontract.ResponseBytes, Deadline: 20 * time.Millisecond, Mutation: mutation, Check: "run_get"}))})
	if err != nil {
		t.Fatal(err)
	}
	rt := Runtime{Now: func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }, Nonce: func() string { return "test" }, Ticker: func(d time.Duration) (<-chan time.Time, func()) { tick := time.NewTicker(d); return tick.C, tick.Stop }}
	s, err := New(Config{FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: secondAuth{}, Registry: reg, Classify: publicerr.Classify, Runtime: rt})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
