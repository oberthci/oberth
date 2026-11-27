package client

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBridgeAppendsDisplayAfterUnchangedJSON(t *testing.T) {
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunFailed, DisplayReason: strings.Repeat("r", 40)}}}
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
		return "renderer block", tool == "run_get" && bytes.Contains(raw, []byte("run1"))
	}
	p := newBridgePeer(t, cfg)
	p.discover()
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
	if len(received.Result.Content) != 2 || !bytes.Equal(received.Result.Content[0], source.Result.Content[0]) || !bytes.Contains(received.Result.Content[1], []byte(`"text":"\nrenderer block"`)) {
		t.Fatal("display replaced or changed original JSON")
	}
	// A host shows the model structured content alone when a reply has it.
	if len(source.Result.Structured) == 0 || len(received.Result.Structured) != 0 {
		t.Fatal("structured content would hide the display block")
	}
}

func TestBridgeListsToolsWithoutOutputSchemas(t *testing.T) {
	h := server(t, fake.Bindings())
	listed := make(chan []byte, 1)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		resp, err := handlerTransport(h).RoundTrip(r)
		if err != nil || req.Method != "tools/list" {
			return resp, err
		}
		data, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		listed <- data
		resp.Body = io.NopCloser(bytes.NewReader(data))
		return resp, err
	}))
	cfg.Display = func(string, json.RawMessage) (string, bool) { return "", false }
	p := newBridgePeer(t, cfg)
	p.discover()
	p.send("list", "tools/list", nil)
	reply := p.frame()
	if !bytes.Contains(<-listed, []byte(`"outputSchema"`)) {
		t.Fatal("server listing has no output schemas to remove")
	}
	var wire struct {
		Result struct{ Tools []map[string]json.RawMessage }
	}
	if json.Unmarshal(reply, &wire) != nil || len(wire.Result.Tools) == 0 || bytes.Contains(reply, []byte(`"outputSchema"`)) {
		t.Fatalf("a listed output schema demands structured content: %.200s", reply)
	}
	for _, tool := range wire.Result.Tools {
		if tool["name"] == nil || tool["inputSchema"] == nil {
			t.Fatal("tool listing lost its names or input schemas")
		}
	}
	p.tool("plain", "repo_list", map[string]any{}, nil)
	if !bytes.Contains(p.frame(), []byte(`"structuredContent"`)) {
		t.Fatal("a reply without a display block lost its structured content")
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
	p.discover()
	p.tool(4, "events_wait", map[string]any{"timeout_s": 600}, nil)
	p.frame()
	if (<-deadline).Before(minimum) {
		t.Fatal("long wait replaced by a short bridge timeout")
	}
}
