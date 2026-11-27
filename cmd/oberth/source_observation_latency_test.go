package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"k8s.io/utils/clock"
)

// This compares a fixed ten-millisecond authentication service, with and
// without the production summary callback, while the log consumer is stalled.
// It establishes instrumentation cost on this fixture, not fleet capacity.
func TestSourceObservationHandlerLatency(t *testing.T) {
	c := &composition{health: &health{}, cacheNotices: make(chan cacheNotice, 128)}
	o := &serverOwners{c: c}
	for range cap(c.cacheNotices) {
		c.cacheNotices <- cacheNotice{}
	}
	cfg := sourcedelivery.Config{Now: (clock.RealClock{}).Now, Authenticate: func(context.Context, string) (sourcedelivery.Caller, error) {
		(clock.RealClock{}).Sleep(10 * time.Millisecond)
		return sourcedelivery.Caller{}, &sourcedelivery.Failure{Code: "forbidden"}
	}, Run: func(context.Context, string) (sourcedelivery.Run, error) { return sourcedelivery.Run{}, nil }, Stage: func(context.Context, sourcecontract.Binding) (sourcedelivery.Staged, error) {
		t.Fatal("forbidden load staged source")
		return sourcedelivery.Staged{}, nil
	}}
	servers := [2]*sourcedelivery.Server{sourcedelivery.New(cfg), nil}
	cfg.Metric = o.sourceObservation
	servers[1] = sourcedelivery.New(cfg)
	var samples [2][]time.Duration
	for i := range 100 {
		for offset := range 2 {
			arm := (i + offset) % 2
			r := httptest.NewRequest(http.MethodPost, sourcedelivery.LoadPath, strings.NewReader(`{"locator":"run","descriptor":{"version":1,"kind":"main"}}`))
			r.Header.Set("Authorization", "Bearer fixture")
			w := httptest.NewRecorder()
			start := (clock.RealClock{}).Now()
			servers[arm].ServeHTTP(w, r)
			samples[arm] = append(samples[arm], (clock.RealClock{}).Since(start))
			if w.Code != http.StatusForbidden {
				t.Fatal("unexpected response", w.Code)
			}
		}
	}
	for arm := range 2 {
		slices.Sort(samples[arm])
	}
	base, observed := samples[0][94], samples[1][94]
	t.Logf("callback disabled p95=%s enabled p95=%s ratio=%.4f", base, observed, float64(observed)/float64(base))
	if observed > base*105/100 {
		t.Fatal("summary callback exceeded five-percent p95 overhead")
	}
	if c.health.source.outcomes[0][2].Load() != 100 || c.health.cacheNoticeDrops.Load() != 100 {
		t.Fatal("latency cohort lost outcomes")
	}
}
