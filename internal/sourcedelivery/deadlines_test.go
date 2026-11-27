package sourcedelivery

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"io"
	"k8s.io/utils/clock"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type stalledHTTP struct {
	header      http.Header
	read, write time.Time
	now         time.Time
	ctx         context.Context
}

func (w *stalledHTTP) Header() http.Header                { return w.header }
func (*stalledHTTP) WriteHeader(int)                      {}
func (w *stalledHTTP) SetReadDeadline(d time.Time) error  { w.read = d; return nil }
func (w *stalledHTTP) SetWriteDeadline(d time.Time) error { w.write = d; return nil }
func (w *stalledHTTP) Write([]byte) (int, error)          { return 0, context.DeadlineExceeded }

func TestBodyAndWriteDeadlinesBoundStalledIOAndTotalDeadline(t *testing.T) {
	now := (clock.RealClock{}).Now()
	for _, total := range []time.Duration{5 * time.Second, 2 * time.Minute} {
		ctx, cancel := context.WithDeadline(t.Context(), now.Add(total))
		w := &stalledHTTP{header: make(http.Header), now: now, ctx: ctx}
		server := New(Config{Now: func() time.Time { return now }})
		req := httptest.NewRequest(http.MethodPost, LoadPath, io.NopCloser(strings.NewReader("")))
		if _, err := server.request(ctx, w, req); err == nil {
			t.Fatal("invalid body accepted")
		}
		wantRead := now.Add(min(total, 10*time.Second))
		if !w.read.Equal(wantRead) {
			t.Fatal("body deadline", w.read, wantRead)
		}
		writer := &deadlineWriter{ctx: ctx, w: w, now: server.config.Now}
		if _, err := writer.Write([]byte("source")); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("write stall accepted", err)
		}
		wantWrite := now.Add(min(total, time.Minute))
		if !w.write.Equal(wantWrite) {
			t.Fatal("write deadline", w.write, wantWrite)
		}
		cancel()
	}
}
func TestAuthenticationDeadlineStopsUnavailableIdentityRead(t *testing.T) {
	var observed time.Duration
	s := New(Config{SourceClientBurst: 3, Authenticate: func(ctx context.Context, _ string) (Caller, error) {
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Error("missing auth deadline")
		}
		observed = deadline.Sub((clock.RealClock{}).Now())
		<-ctx.Done()
		return Caller{}, context.Cause(ctx)
	}})
	ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
	defer cancel()
	_, _, _, err := s.authorize(ctx, "Bearer bound-token", LoadRequest{})
	if !errors.Is(err, context.DeadlineExceeded) || observed > time.Millisecond {
		t.Fatal("auth ignored total deadline", err, observed)
	}
}

type cancelBody struct{ cancel context.CancelFunc }

func (b *cancelBody) Read(p []byte) (int, error) { b.cancel(); return 0, io.EOF }
func (*cancelBody) Close() error                 { return nil }
func TestCancelledStreamHasNoReceiptAndForbiddenCountsMetric(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	counted := false
	server := New(Config{Now: (clock.RealClock{}).Now, Metric: func(summary Summary) {
		if summary.Code == "forbidden" {
			counted = true
		}
	}})
	w := httptest.NewRecorder()
	server.send(ctx, w, transfer{stage: Staged{Body: &cancelBody{cancel: cancel}}})
	if w.Header().Get(ReceiptTrailer) != "" {
		t.Fatal("cancelled stream got a receipt")
	}
	server.config.Authenticate = func(context.Context, string) (Caller, error) { return Caller{}, &Failure{Code: "forbidden"} }
	server.config.Run = func(context.Context, string) (Run, error) { return Run{}, nil }
	server.config.Stage = func(context.Context, sourcecontract.Binding) (Staged, error) {
		t.Fatal("forbidden request staged source")
		return Staged{}, nil
	}
	req := httptest.NewRequest(http.MethodPost, LoadPath, strings.NewReader(`{"locator":"run","descriptor":{"version":1,"kind":"main"}}`))
	req.Header.Set("Authorization", "Bearer bound")
	server.ServeHTTP(httptest.NewRecorder(), req)
	if !counted {
		t.Fatal("forbidden response not counted")
	}
}
