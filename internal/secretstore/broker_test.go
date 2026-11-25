package secretstore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBrokerCapacityRetriesAreBoundedByCallerAndTransportDeadline(t *testing.T) {
	for _, bound := range []time.Duration{3500 * time.Millisecond, 30 * time.Second} {
		t.Run(bound.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				calls := 0
				c, err := New(Connection{URL: "https://broker.invalid", Transport: roundTrip(func(*http.Request) (*http.Response, error) {
					calls++
					return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": []string{"1"}}, Body: io.NopCloser(strings.NewReader("private reply"))}, nil
				})})
				if err != nil {
					t.Fatal(err)
				}
				ctx := t.Context()
				if bound < 30*time.Second {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, bound)
					defer cancel()
				}
				start := time.Now() //nolint:forbidigo // synctest owns this clock; no wall time elapses.
				_, err = c.fetchBroker(ctx, []byte("private fixture"), []string{"registry/value"})
				elapsed := time.Since(start) //nolint:forbidigo // Measure the clock controlled by synctest.
				if err == nil || elapsed != bound || calls < 2 || calls > 9 {
					t.Fatal("unbounded or absent capacity retry", calls, elapsed, err)
				}
				if publicerr.Classify(err).Reason() != "Secret delivery is unavailable." {
					t.Fatal("capacity used sealed diagnosis", err)
				}
			})
		})
	}
}

func TestBrokerRefusalsPreserveRecoveryAndRejectMissingValues(t *testing.T) {
	for _, test := range []struct {
		status       int
		body, reason string
	}{
		{502, `{"private":"reply"}`, "Secret delivery is unavailable."},
		{503, `{"private":"reply"}`, "Secret store is sealed."},
		{404, `{"private":"reply"}`, "No value is stored for this path."},
		{200, `{}`, "Secret store returned an invalid response."},
	} {
		c, err := New(Connection{URL: "https://delivery.invalid", Transport: roundTrip(func(r *http.Request) (*http.Response, error) {
			if r.URL.Path != BrokerPath || r.Header.Get("X-Vault-Token") != "verified-pod-jwt" {
				t.Error("broker sent another endpoint or identity")
			}
			return &http.Response{StatusCode: test.status, Body: io.NopCloser(strings.NewReader(test.body))}, nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		values, err := c.fetchBroker(t.Context(), []byte("verified-pod-jwt"), []string{"registry/value"})
		var p *publicerr.Error
		if values != nil || !errors.As(err, &p) || p.Reason() != test.reason || strings.Contains(p.Error(), "private") {
			t.Fatal("broker lost refusal or leaked response", p)
		}
		if publicerr.CheckMessage(p.Reason(), 80) != nil || publicerr.CheckMessage(p.Remedy(), 120) != nil {
			t.Fatal("broker diagnostic exceeds its bound")
		}
		c.Close()
	}
}
