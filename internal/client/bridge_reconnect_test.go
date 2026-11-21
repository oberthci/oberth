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
)

func TestBridgeInitializeInstructionsUnchanged(t *testing.T) {
	h := server(t, fake.Bindings())
	original := make(chan []byte, 1)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		resp, err := handlerTransport(h).RoundTrip(r)
		if err != nil {
			return nil, err
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
	p := newBridgePeer(t, cfg)
	var received, source struct{ Result struct{ Instructions string } }
	if json.Unmarshal(p.initialize(), &received) != nil || json.Unmarshal(<-original, &source) != nil || received.Result.Instructions != source.Result.Instructions || source.Result.Instructions == "" {
		t.Fatal("server instruction text changed")
	}
}

func TestBridgeSafeReadReconnects(t *testing.T) {
	h := server(t, fake.Bindings())
	var calls, waits atomic.Int32
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method == "tools/call" && calls.Add(1) == 1 {
			return &http.Response{StatusCode: 503, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":"unavailable","reason":"Server is restarting.","remedy":"Read state after reconnecting."}`)), Request: r}, nil
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	cfg.Config.Wait = func(ctx context.Context, _ time.Duration) error { waits.Add(1); return ctx.Err() }
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool("read", "repo_list", map[string]any{}, nil)
	var reply struct {
		ID    string
		Error any
	}
	if json.Unmarshal(p.frame(), &reply) != nil || reply.ID != "read" || reply.Error != nil || calls.Load() != 2 || waits.Load() != 1 {
		t.Fatal("safe read did not use the existing reconnect policy")
	}
}

func TestBridgeInitializationNotificationDoesNotBlockRequests(t *testing.T) {
	entered := make(chan struct{})
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method == "notifications/initialized" {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send(nil, "notifications/initialized", nil)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("notification not forwarded")
	}
	p.send("fast", "ping", nil)
	if !bytes.Contains(p.frame(), []byte(`"id":"fast"`)) {
		t.Fatal("initialization notification blocked a later call")
	}
}
