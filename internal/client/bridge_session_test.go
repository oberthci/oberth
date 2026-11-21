package client

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestBridgeClosesSessionOnEOFAndCancellation(t *testing.T) {
	for _, interrupted := range []bool{false, true} {
		t.Run(map[bool]string{false: "EOF", true: "cancelled"}[interrupted], func(t *testing.T) {
			checkBridgeShutdown(t, interrupted)
		})
	}
}

func TestBridgeShutdownAcceptsClosedSession(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusNoContent, http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError} {
		for _, interrupted := range []bool{false, true} {
			t.Run(http.StatusText(status)+map[bool]string{false: "/EOF", true: "/cancelled"}[interrupted], func(t *testing.T) {
				h := server(t, fake.Bindings())
				closed := make(chan http.Header, 1)
				cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodDelete {
						closed <- r.Header.Clone()
						return &http.Response{StatusCode: status, Body: http.NoBody}, nil
					}
					return handlerTransport(h).RoundTrip(r)
				}))
				var diagnostic bytes.Buffer
				cfg.Diagnostic = &diagnostic
				p := newBridgePeer(t, cfg)
				p.initialize()
				finishBridgePeer(t, p, interrupted, closed)
				wantDiagnostic := !interrupted && (status == http.StatusForbidden || status == http.StatusInternalServerError)
				if (diagnostic.Len() != 0) != wantDiagnostic {
					t.Fatalf("shutdown status %d: diagnostic=%q", status, diagnostic.String())
				}
			})
		}
	}
}

func TestBridgeRetriesCapacityRefusalWithBackoff(t *testing.T) {
	var out bytes.Buffer
	var delays []time.Duration
	gets := 0
	cfg := Config{Options: Options{URL: "https://127.0.0.1"}, Now: fake.NewClock().Now}
	cfg.Wait = func(_ context.Context, d time.Duration) error { delays = append(delays, d); return nil }
	cfg.Transport = roundTrip(func(*http.Request) (*http.Response, error) {
		gets++
		if gets <= 7 {
			return &http.Response{StatusCode: http.StatusTooManyRequests, Body: http.NoBody}, nil
		}
		if gets == 8 {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events\"}\n\n"))}, nil
		}
		return &http.Response{StatusCode: http.StatusForbidden, Body: http.NoBody}, nil
	})
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	b := bridge{client: c, session: "session", out: &out, config: BridgeConfig{Jitter: func() float64 { return 0 }}}
	b.followEvents(t.Context())
	want := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second, 30 * time.Second, 2 * time.Second}
	if gets != 9 || !reflect.DeepEqual(delays, want) || !strings.Contains(out.String(), "notifications/events") {
		t.Fatalf("capacity retry: requests=%d delays=%v output=%s", gets, delays, &out)
	}
}

func TestBridgePreservesSessionLimitRemedy(t *testing.T) {
	reply := `{"jsonrpc":"2.0","id":"init","error":{"code":-32000,"message":"This identity has reached its session limit.","data":{"reason":"This identity has reached its session limit.","remedy":"Close an unused session with DELETE /mcp or retry after idle sessions expire."}}}`
	cfg := bridgeProfile(t, roundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply))}, nil
	}))
	p := newBridgePeer(t, cfg)
	p.send("init", "initialize", map[string]string{"protocolVersion": apicontract.ProtocolVersion})
	if got := p.frame(); string(got) != reply {
		t.Fatal("server's session limit or remedy was changed", string(got))
	}
}

func checkBridgeShutdown(t *testing.T, interrupted bool) {
	t.Helper()
	h := server(t, fake.Bindings())
	closed := make(chan http.Header, 1)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			if r.Context().Err() != nil {
				t.Error("session close inherited cancellation")
			}
			if _, ok := r.Context().Deadline(); !ok {
				t.Error("session close has no deadline")
			}
			closed <- r.Header.Clone()
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	for range 20 {
		p := newBridgePeer(t, cfg)
		p.initialize()
		finishBridgePeer(t, p, interrupted, closed)
	}
}

func finishBridgePeer(t *testing.T, p *bridgePeer, interrupted bool, closed <-chan http.Header) {
	t.Helper()
	if interrupted {
		p.cancel()
	} else {
		_ = p.input.Close()
	}
	select {
	case header := <-closed:
		if header.Get("Mcp-Session-Id") == "" || header.Get("MCP-Protocol-Version") != apicontract.ProtocolVersion || header.Get("Authorization") != "Bearer valid" {
			t.Fatal("session close lost identity or session", header)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not close its session")
	}
	select {
	case err := <-p.done:
		if !interrupted && err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("bridge did not finish shutdown")
	}
}
