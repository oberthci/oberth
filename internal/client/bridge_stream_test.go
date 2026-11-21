package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

type untilCancelled struct{ ctx context.Context }

func (r untilCancelled) Read([]byte) (int, error) { <-r.ctx.Done(); return 0, r.ctx.Err() }

func TestBridgeForwardsEventsAndResumes(t *testing.T) {
	first := `{ "jsonrpc":"2.0", "method":"notifications/events", "params":{"next_after_id":41} }`
	second := `{"jsonrpc":"2.0","method":"notifications/resources/updated","params":{"uri":"run://run1"}}`
	h := server(t, fake.Bindings())
	var gets atomic.Int32
	headers := make(chan http.Header, 2)
	waits := make(chan time.Duration, 2)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return handlerTransport(h).RoundTrip(r)
		}
		headers <- r.Header.Clone()
		frame := "id: 41\ndata: " + first + "\n\n"
		var body io.Reader = strings.NewReader(frame)
		if gets.Add(1) == 2 {
			body = io.MultiReader(strings.NewReader("id: 42\ndata: "+second+"\n\n"), untilCancelled{r.Context()})
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(body), Request: r}, nil
	}))
	cfg.Config.Wait = func(ctx context.Context, d time.Duration) error { waits <- d; return ctx.Err() }
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send(nil, "notifications/initialized", nil)
	if string(p.frame()) != first || string(p.frame()) != second {
		t.Fatal("pushed protocol frames changed")
	}
	initial, resumed := <-headers, <-headers
	if initial.Get("Mcp-Session-Id") == "" || resumed.Get("Mcp-Session-Id") != initial.Get("Mcp-Session-Id") || resumed.Get("Last-Event-ID") != "41" || resumed.Get("MCP-Protocol-Version") != apicontract.ProtocolVersion {
		t.Fatal("session or resume cursor missing")
	}
	if delay := <-waits; delay < 2*time.Second || delay > 30*time.Second {
		t.Fatal("event reconnect delay outside contract")
	}
}

func TestBridgeAppendsDisplayAfterUnchangedJSON(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunFailed, DisplayReason: strings.Repeat("r", 40)}}
	h := server(t, b)
	original := make(chan []byte, 1)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		resp, err := handlerTransport(h).RoundTrip(r)
		if err != nil || req.Method != "tools/call" {
			return resp, err
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			return nil, err
		}
		original <- data
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp, nil
	}))
	cfg.Display = func(tool string, raw json.RawMessage) (string, bool) {
		if tool != "run_get" || !bytes.Contains(raw, []byte("run1")) {
			return "", false
		}
		return "renderer block", true
	}
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool("display", "run_get", map[string]any{"id": "run1"}, nil)
	var received, source struct {
		Result struct {
			Content    []json.RawMessage `json:"content"`
			Structured json.RawMessage   `json:"structuredContent"`
		}
	}
	if json.Unmarshal(p.frame(), &received) != nil || json.Unmarshal(<-original, &source) != nil {
		t.Fatal("bad protocol")
	}
	if len(received.Result.Content) != 2 || !bytes.Equal(received.Result.Content[0], source.Result.Content[0]) || !bytes.Equal(received.Result.Structured, source.Result.Structured) || !bytes.Contains(received.Result.Content[1], []byte("renderer block")) {
		t.Fatal("display replaced or changed original JSON")
	}
}

func TestBridgeUsesDeclaredWaitBudget(t *testing.T) {
	long, cancel := context.WithTimeout(t.Context(), 600*time.Second)
	defer cancel()
	minimum, _ := long.Deadline()
	h := server(t, fake.Bindings())
	deadline := make(chan time.Time, 1)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method == "tools/call" {
			value, _ := r.Context().Deadline()
			deadline <- value
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool(4, "events_wait", map[string]any{"timeout_s": 600}, nil)
	p.frame()
	if (<-deadline).Before(minimum) {
		t.Fatal("long wait replaced by a short bridge timeout")
	}
}
