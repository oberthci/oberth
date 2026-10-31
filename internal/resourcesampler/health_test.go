package resourcesampler

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestScrapeOutagesAreNodeScopedRearmedAndCountSharedFills(t *testing.T) {
	var mode, requests atomic.Int32
	c, server, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch mode.Load() {
		case 0:
			http.Error(w, "response-secret-canary", http.StatusForbidden)
		case 1, 2:
			if r.URL.Path == "/stats/summary" {
				fmt.Fprint(w, `{"node":{"nodeName":"node-a"},"pods":[]}`)
			} else if mode.Load() == 1 {
				fmt.Fprint(w, "container_memory_usage_bytes NaN 1\n")
			}
		}
	})
	c.config.Nodes["node-b"] = server.URL
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	c.now = func() time.Time { return time.Unix(0, clock.Load()) }
	messages := make(chan string, 64)
	c.logf = func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }
	wantLog := func(part string) {
		t.Helper()
		select {
		case message := <-messages:
			if !strings.Contains(message, part) || strings.Contains(message, "canary") || strings.Contains(message, "first-token") || strings.Contains(message, server.URL) {
				t.Fatalf("unsafe or incorrect diagnostic: %s", message)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("missing scrape transition", part)
		}
	}
	snapshot := func(node string) Snapshot {
		t.Helper()
		value, err := c.Snapshot(t.Context(), node)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	wantMetrics := func(failures, unavailable int) {
		t.Helper()
		before := requests.Load()
		var out bytes.Buffer
		c.WriteMetrics(&out)
		for _, expected := range []string{
			"# TYPE oberth_resource_sampler_scrape_failures_total counter\n",
			fmt.Sprintf("oberth_resource_sampler_scrape_failures_total %d\n", failures),
			"# TYPE oberth_resource_sampler_nodes_unavailable gauge\n",
			fmt.Sprintf("oberth_resource_sampler_nodes_unavailable %d\n", unavailable),
		} {
			if !strings.Contains(out.String(), expected) {
				t.Fatalf("missing %q: %s", expected, out.String())
			}
		}
		if requests.Load() != before || strings.Contains(out.String(), "node-") {
			t.Fatal("metrics did IO or introduced node labels", out.String())
		}
	}
	wantMetrics(0, 0)
	var callers sync.WaitGroup
	for range 20 {
		callers.Go(func() {
			value, err := c.Snapshot(t.Context(), "node-a")
			if err != nil || value.SummaryOK || value.MetricsOK {
				t.Errorf("failed scrape changed snapshot contract: %#v %v", value, err)
			}
		})
	}
	callers.Wait()
	wantLog(`node="node-a" scrape incomplete summary_ok=false metrics_ok=false`)
	if requests.Load() != 2 {
		t.Fatal("shared scrape duplicated", requests.Load())
	}
	wantMetrics(1, 1)
	snapshot("node-b")
	wantLog(`node="node-b" scrape incomplete`)
	wantMetrics(2, 2)
	// Another fill in the same outage increments the counter but does not warn.
	clock.Add(int64(time.Second))
	snapshot("node-a")
	wantMetrics(3, 2)
	// Partial recovery (valid summary, bad cAdvisor counter) is still an outage.
	mode.Store(1)
	clock.Add(int64(time.Second))
	value := snapshot("node-a")
	if !value.SummaryOK || value.MetricsOK {
		t.Fatal("partial success discarded or invalid metrics accepted", value)
	}
	wantMetrics(4, 2)
	mode.Store(2)
	clock.Add(int64(time.Second))
	value = snapshot("node-a")
	if !value.SummaryOK || !value.MetricsOK {
		t.Fatal("recovery missing", value)
	}
	wantLog(`node="node-a" scrape recovered`)
	wantMetrics(4, 1)
	// Only full recovery re-arms the warning; another node stays unavailable.
	mode.Store(0)
	clock.Add(int64(time.Second))
	snapshot("node-a")
	wantLog(`node="node-a" scrape incomplete`)
	wantMetrics(5, 2)
	select {
	case message := <-messages:
		t.Fatal("duplicate outage warning", message)
	default:
	}
}

func TestScrapeGateTimeoutIsVisibleWithoutRequests(t *testing.T) {
	c, _, _ := testClient(t, func(http.ResponseWriter, *http.Request) { t.Error("gate timeout reached kubelet") })
	// Deterministically exhaust the shared fill budget while it waits for a slot.
	for range cap(c.gate) {
		c.gate <- struct{}{}
	}
	messages := make(chan string, 1)
	c.logf = func(format string, args ...any) { messages <- fmt.Sprintf(format, args...) }
	ctx, cancel := context.WithTimeout(t.Context(), 6*time.Second)
	defer cancel()
	value, err := c.Snapshot(ctx, "node-a")
	if err != nil || value.SummaryOK || value.MetricsOK {
		t.Fatalf("gate timeout: %#v %v", value, err)
	}
	select {
	case message := <-messages:
		if !strings.Contains(message, "scrape incomplete") {
			t.Fatal(message)
		}
	case <-ctx.Done():
		t.Fatal("gate timeout not logged")
	}
	var out bytes.Buffer
	c.WriteMetrics(&out)
	if !strings.Contains(out.String(), "oberth_resource_sampler_scrape_failures_total 1\n") {
		t.Fatal(out.String())
	}
}
