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
)

func TestBridgeEventFramesResetReconnectDelay(t *testing.T) {
	frame := "data: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/events\"}\n\n"
	for _, scenario := range []struct {
		name   string
		bodies []string
		waits  []time.Duration
	}{
		{"frames-between-drops", []string{frame, frame}, []time.Duration{2 * time.Second, 2 * time.Second}},
		{"empty-drop-before-frame", []string{"", "", frame}, []time.Duration{2 * time.Second, 4 * time.Second, 2 * time.Second}},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			var out bytes.Buffer
			var waits []time.Duration
			gets := 0
			cfg := Config{Options: Options{URL: "https://127.0.0.1"}, Now: fake.NewClock().Now}
			cfg.Wait = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
			cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
				gets++
				if r.Method != http.MethodGet || gets > len(scenario.bodies)+1 {
					t.Fatal("unexpected event request")
				}
				if gets == len(scenario.bodies)+1 {
					return &http.Response{StatusCode: 403, Body: http.NoBody}, nil
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(scenario.bodies[gets-1]))}, nil
			})
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			b := bridge{client: c, session: "session", out: &out, config: BridgeConfig{Jitter: func() float64 { return 0 }}}
			b.followEvents(t.Context())
			if gets != len(scenario.bodies)+1 || !reflect.DeepEqual(waits, scenario.waits) {
				t.Fatalf("reconnects=%d delays=%v; want %v", gets, waits, scenario.waits)
			}
			if strings.Count(out.String(), "notifications/events") != strings.Count(strings.Join(scenario.bodies, ""), "notifications/events") {
				t.Fatal("event frames lost between drops")
			}
		})
	}
}

func TestBridgeEventStreamRequiresSession(t *testing.T) {
	for _, session := range []string{"", "session"} {
		t.Run("session="+session, func(t *testing.T) {
			gets := 0
			cfg := Config{Options: Options{URL: "https://127.0.0.1"}, Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
				gets++
				if r.Method != http.MethodGet || r.Header.Get("Mcp-Session-Id") != session {
					t.Fatal("unexpected stream request")
				}
				return &http.Response{StatusCode: 403, Body: http.NoBody}, nil
			})}
			c, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			b := bridge{client: c, session: session}
			b.followEvents(t.Context())
			want := 0
			if session != "" {
				want = 1
			}
			if gets != want {
				t.Fatalf("stream requests=%d, want %d", gets, want)
			}
		})
	}
}
