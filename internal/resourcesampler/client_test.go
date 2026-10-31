package resourcesampler

import (
	"context"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server, string) {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	root := t.TempDir()
	ca := filepath.Join(root, "ca.crt")
	token := filepath.Join(root, "token")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(token, []byte("first-token"), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(Config{Nodes: map[string]string{"node-a": server.URL}, CAFile: ca, TokenFile: token, Interval: "1s"})
	if err != nil {
		t.Fatal(err)
	}
	return c, server, token
}
func TestResourceSharedFillSurvivesWaiterCancellationAndRotatesToken(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var requests atomic.Int32
	var rotated atomic.Bool
	c, _, token := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		once.Do(func() { close(started) })
		<-release
		if r.Header.Get("Authorization") == "Bearer second-token" {
			rotated.Store(true)
		}
		if r.URL.Path == "/stats/summary" {
			fmt.Fprint(w, `{"node":{"nodeName":"node-a"},"pods":[]}`)
		} else {
			fmt.Fprintf(w, "container_memory_usage_bytes{container=\"main\"} 0 %d\n", time.Now().UnixMilli())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := c.Snapshot(ctx, "node-a"); done <- err }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("waiter %v", err)
	}
	if err := os.WriteFile(token, []byte("second-token"), 0600); err != nil {
		t.Fatal(err)
	}
	var wait sync.WaitGroup
	for range 20 {
		wait.Go(func() {
			snapshot, err := c.Snapshot(context.Background(), "node-a")
			if err != nil || !snapshot.SummaryOK || !snapshot.MetricsOK {
				t.Errorf("shared snapshot %#v %v", snapshot, err)
			}
		})
	}
	close(release)
	wait.Wait()
	if requests.Load() != 2 || !rotated.Load() {
		t.Fatalf("coalescing/rotation: requests=%d rotated=%v", requests.Load(), rotated.Load())
	}
	if _, err := c.Snapshot(context.Background(), "unconfigured"); err == nil {
		t.Fatal("unconfigured endpoint accepted")
	}
}
func TestResourceTransportRejectsRedirectsBadTLSAndOversize(t *testing.T) {
	for _, kind := range []string{"redirect", "oversize", "status", "wrong-node"} {
		t.Run(kind, func(t *testing.T) {
			var forwarded atomic.Int32
			sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { forwarded.Add(1) }))
			defer sink.Close()
			c, _, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch kind {
				case "redirect":
					http.Redirect(w, r, sink.URL, http.StatusFound)
				case "oversize":
					fmt.Fprint(w, strings.Repeat("x", MaxResponseBytes+1))
				case "status":
					http.Error(w, "secret-canary", http.StatusForbidden)
				case "wrong-node":
					fmt.Fprint(w, `{"node":{"nodeName":"other"}}`)
				}
			})
			snapshot, err := c.Snapshot(context.Background(), "node-a")
			if err != nil || snapshot.SummaryOK || snapshot.MetricsOK && kind != "wrong-node" {
				t.Fatalf("accepted invalid response %#v %v", snapshot, err)
			}
			if forwarded.Load() != 0 {
				t.Fatal("credential redirect followed")
			}
		})
	}
	c, server, _ := testClient(t, func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, `{"node":{"nodeName":"node-a"}}`) })
	other := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	defer other.Close()
	// Different test servers use the same fixture certificate; using a hostname
	// absent from the pinned certificate exercises mandatory hostname checking.
	c.config.Nodes["node-a"] = strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	snapshot, _ := c.Snapshot(context.Background(), "node-a")
	if snapshot.SummaryOK {
		t.Fatal("TLS hostname was not checked")
	}
}
func TestResourceGlobalScrapeGateBounded(t *testing.T) {
	var active, peak atomic.Int32
	c, server, _ := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old; old = peak.Load() {
			if peak.CompareAndSwap(old, n) {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
		if r.URL.Path == "/stats/summary" {
			fmt.Fprint(w, `{"node":{"nodeName":"node-a"}}`)
		}
	})
	c.http.Transport.(*http.Transport).MaxConnsPerHost = 0
	for i := 0; i < 8; i++ {
		c.config.Nodes[fmt.Sprintf("node-%d", i)] = server.URL
	}
	var wait sync.WaitGroup
	for i := 0; i < 8; i++ {
		wait.Go(func() { _, _ = c.Snapshot(context.Background(), fmt.Sprintf("node-%d", i)) })
	}
	wait.Wait()
	if peak.Load() > 2 || peak.Load() < 1 {
		t.Fatalf("scrape concurrency %d", peak.Load())
	}
}
