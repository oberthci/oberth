// Package resourcesampler measures outside untrusted CI containers. It never
// reads termination messages, user-defined metrics or container-hosted endpoints.
package resourcesampler

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const MaxResponseBytes = 16 << 20
const MaxSampleAge = 30 * time.Second
const DefaultInterval = 15 * time.Second

type Config struct {
	Nodes     map[string]string `json:"nodes"`
	CAFile    string            `json:"ca_file"`
	TokenFile string            `json:"token_file"`
	Interval  string            `json:"interval,omitempty"`
}

func ReadConfig(name string) (Config, error) {
	var c Config
	if !filepath.IsAbs(name) {
		return c, errors.New("resource sampler config must be absolute")
	}
	f, err := os.Open(name) //nolint:gosec // Administrator-owned explicit configuration path.
	if err != nil {
		return c, errors.New("resource sampler config unavailable")
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	if err != nil || len(raw) > 256<<10 {
		return c, errors.New("resource sampler config exceeds bound")
	}
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil || d.Decode(&struct{}{}) != io.EOF {
		return c, errors.New("invalid resource sampler config")
	}
	return c, c.Validate()
}
func (c Config) PollInterval() time.Duration {
	if c.Interval == "" {
		return DefaultInterval
	}
	d, _ := time.ParseDuration(c.Interval)
	return d
}
func (c Config) Validate() error {
	bad := errors.New("invalid resource sampler endpoints, credentials or interval")
	if len(c.Nodes) == 0 || len(c.Nodes) > 32 || !filepath.IsAbs(c.CAFile) || !filepath.IsAbs(c.TokenFile) || c.PollInterval() < time.Second || c.PollInterval() > 30*time.Second {
		return bad
	}
	for node, endpoint := range c.Nodes {
		if node == "" || len(node) > 253 || strings.ContainsAny(node, "/\\\x00\r\n\t ") {
			return bad
		}
		u, err := url.Parse(endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return bad
		}
	}
	return nil
}
func (c Config) NodeNames() []string {
	out := make([]string, 0, len(c.Nodes))
	for n := range c.Nodes {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

type Snapshot struct {
	Summary              Summary
	Metrics              []Metric
	SummaryOK, MetricsOK bool
}
type cacheEntry struct {
	at    time.Time
	ready chan struct{}
	value Snapshot
}
type Client struct {
	config Config
	http   *http.Client
	gate   chan struct{}
	mu     sync.Mutex
	cache  map[string]*cacheEntry
	now    func() time.Time
	logf   func(string, ...any)
	// Bounded by the configured nodes, independent of runs and cache entries.
	unavailable map[string]bool
	failures    uint64
}

func NewClient(c Config) (*Client, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	caFile, err := os.Open(c.CAFile) //nolint:gosec // Administrator-owned explicit CA path.
	if err != nil {
		return nil, errors.New("resource sampler CA unavailable")
	}
	ca, err := io.ReadAll(io.LimitReader(caFile, (1<<20)+1))
	_ = caFile.Close()
	if err != nil || len(ca) > 1<<20 {
		return nil, errors.New("resource sampler CA unavailable or oversized")
	}
	c.Nodes = maps.Clone(c.Nodes)
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, errors.New("invalid resource sampler CA")
	}
	tr := &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 2 * time.Second, KeepAlive: 15 * time.Second}).DialContext, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 2 * time.Second, MaxResponseHeaderBytes: 64 << 10, MaxIdleConns: 32, MaxConnsPerHost: 2, IdleConnTimeout: 30 * time.Second}
	return &Client{config: c, http: &http.Client{Transport: tr, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, gate: make(chan struct{}, 2), cache: map[string]*cacheEntry{}, now: time.Now, logf: log.Printf, unavailable: map[string]bool{}}, nil
}

// A shared scrape has its own bounded lifetime. One cancelled run cannot
// cancel the scrape serving other runs on that node. At most two nodes scrape.
func (c *Client) Snapshot(ctx context.Context, node string) (Snapshot, error) {
	if _, ok := c.config.Nodes[node]; !ok {
		return Snapshot{}, errors.New("resource sampler node is not configured")
	}
	c.mu.Lock()
	entry := c.cache[node]
	if entry == nil || c.now().Sub(entry.at) >= c.config.PollInterval() {
		if entry == nil || entry.ready == nil {
			entry = &cacheEntry{at: c.now(), ready: make(chan struct{})}
			c.cache[node] = entry
			go c.fill(node, entry) //nolint:gosec // Node-scoped fill has its own four-second lifetime and must not inherit a waiter or its context values.
		}
	}
	ready := entry.ready
	if ready == nil {
		value := entry.value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return Snapshot{}, ctx.Err()
	case <-ready:
	}
	c.mu.Lock()
	value := entry.value
	c.mu.Unlock()
	return value, nil
}
func (c *Client) fill(node string, entry *cacheEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	var value Snapshot
	select {
	case c.gate <- struct{}{}:
		defer func() { <-c.gate }()
	case <-ctx.Done():
		c.finish(node, entry, value)
		return
	}
	if raw, err := c.get(ctx, node, "/stats/summary"); err == nil {
		if json.Unmarshal(raw, &value.Summary) == nil && value.Summary.Node.NodeName == node && len(value.Summary.Pods) <= 4096 {
			value.SummaryOK = true
		}
	}
	if raw, err := c.get(ctx, node, "/metrics/cadvisor"); err == nil {
		if metrics, err := ParseMetrics(raw); err == nil {
			value.Metrics = metrics
			value.MetricsOK = true
		}
	}
	c.finish(node, entry, value)
}
func (c *Client) finish(node string, entry *cacheEntry, value Snapshot) {
	c.mu.Lock()
	failed := !value.SummaryOK || !value.MetricsOK
	wasUnavailable := c.unavailable[node]
	if failed {
		c.failures++
		c.unavailable[node] = true
	} else {
		delete(c.unavailable, node)
	}
	entry.value = value
	entry.at = c.now()
	close(entry.ready)
	entry.ready = nil
	c.mu.Unlock()
	// Only fixed diagnostic fields, never credentials, URLs, response bodies or
	// transport/parse errors. Log and metric IO cannot hold the cache mutex.
	if failed && !wasUnavailable {
		c.logf("WARN: external resource sampler node=%q scrape incomplete summary_ok=%t metrics_ok=%t; missing usage remains unknown", node, value.SummaryOK, value.MetricsOK)
	} else if !failed && wasUnavailable {
		c.logf("external resource sampler node=%q scrape recovered", node)
	}
}

// WriteMetrics reports completed shared scrapes without IO to kubelets/storage.
// A partial scrape is one failure, not one per endpoint, waiter or run.
func (c *Client) WriteMetrics(w io.Writer) {
	c.mu.Lock()
	failures, unavailable := c.failures, len(c.unavailable)
	c.mu.Unlock()
	_, _ = fmt.Fprintf(w, "# TYPE oberth_resource_sampler_scrape_failures_total counter\noberth_resource_sampler_scrape_failures_total %d\n# TYPE oberth_resource_sampler_nodes_unavailable gauge\noberth_resource_sampler_nodes_unavailable %d\n", failures, unavailable)
}
func (c *Client) get(ctx context.Context, node, path string) ([]byte, error) {
	// Re-read projected tokens so rotation is honoured; never use a caller's
	// uplink or forward credentials to a redirect/proxy/unconfigured node.
	f, err := os.Open(c.config.TokenFile) //nolint:gosec // Administrator-owned rotating token path.
	if err != nil {
		return nil, errors.New("sampler credential unavailable")
	}
	token, err := io.ReadAll(io.LimitReader(f, 16385))
	_ = f.Close()
	text := strings.TrimSpace(string(token))
	if err != nil || len(token) > 16384 || text == "" || strings.ContainsAny(text, "\r\n\t ") {
		return nil, errors.New("invalid sampler credential")
	}
	endpoint := strings.TrimRight(c.config.Nodes[node], "/") + path
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Authorization", "Bearer "+text)
	response, err := c.http.Do(request)
	if err != nil {
		return nil, errors.New("kubelet sampling unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("kubelet sampling refused")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, MaxResponseBytes+1))
	if err != nil || len(raw) > MaxResponseBytes {
		return nil, errors.New("kubelet sampling response exceeds bound")
	}
	return raw, nil
}

func (c *Client) Interval() time.Duration { return c.config.PollInterval() }
