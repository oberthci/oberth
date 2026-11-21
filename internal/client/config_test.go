package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
)

func TestZeroConfigClockAndWait(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		cfg := Config{}
		before := cfg.Time()
		if before.IsZero() {
			t.Fatal("zero configuration has no clock")
		}
		if err := cfg.WaitFor(t.Context(), 5*time.Second); err != nil {
			t.Fatal(err)
		}
		if elapsed := cfg.Time().Sub(before); elapsed != 5*time.Second {
			t.Fatal("default wait did not use its delay", elapsed)
		}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- cfg.WaitFor(ctx, time.Hour) }()
		synctest.Wait()
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatal("default wait ignored cancellation", err)
		}
	})
}

func TestConfigUsesInjectedClockAndWait(t *testing.T) {
	want := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	sentinel := errors.New("injected wait")
	ctx := t.Context()
	cfg := Config{Now: func() time.Time { return want }, Wait: func(got context.Context, delay time.Duration) error {
		if got != ctx || delay != time.Hour {
			t.Fatal("wait arguments changed", delay)
		}
		return sentinel
	}}
	if cfg.Time() != want || !errors.Is(cfg.WaitFor(ctx, time.Hour), sentinel) {
		t.Fatal("injected callbacks were replaced")
	}
}

func TestClientDefaultsHandleSafeReadRetry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		handler := handlerTransport(server(t, fake.Bindings()))
		calls := 0
		cfg := Config{Options: Options{URL: "https://127.0.0.1", TokenFile: privateFile(t, "token", []byte("valid"))}}
		// A lost safe read exercises the default clock and wait.
		cfg.Transport = roundTrip(func(r *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				return nil, io.EOF
			}
			return handler.RoundTrip(r)
		})
		c, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, refused, err := c.Call(t.Context(), definition(t, "repo_list"), json.RawMessage(`{}`))
		if err != nil || refused || calls != 2 {
			t.Fatal("zero callbacks could not reconnect", calls, refused, err)
		}
	})
}
