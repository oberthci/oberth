package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestBridgeNoProfile(t *testing.T) {
	var calls atomic.Int32
	cfg := BridgeConfig{Base: filepath.Join(t.TempDir(), "absent"), Config: Config{Transport: roundTrip(func(*http.Request) (*http.Response, error) { calls.Add(1); return nil, io.EOF })}}
	var out bytes.Buffer
	err := Bridge(t.Context(), strings.NewReader(""), &out, cfg)
	if err == nil || !strings.Contains(err.Error(), "oberth join <file>") || calls.Load() != 0 || out.Len() != 0 {
		t.Fatalf("missing selection did not refuse before I/O: %v", err)
	}
}

func TestBridgeConcurrentRequests(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	b := fake.Bindings()
	b.EventsWait.Handler = &fake.Handler[apicontract.EventsWaitArgs, apicontract.EventResult]{Run: func(ctx context.Context) (apicontract.EventResult, error) {
		close(entered)
		<-ctx.Done()
		close(cancelled)
		return apicontract.EventResult{}, ctx.Err()
	}}
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run1", Status: stepmodel.RunFailed}}}
	h := server(t, b)
	p := newBridgePeer(t, bridgeProfile(t, streamingTransport(h)))
	init := p.initialize()
	if !bytes.Contains(init, []byte("once per code change")) {
		t.Fatal("server instructions changed")
	}
	p.tool(7, "events_wait", map[string]any{"timeout_s": 600}, nil)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("long wait never reached the server")
	}
	p.tool("fast", "run_get", map[string]any{"id": "run1"}, nil)
	if !bytes.Contains(p.frame(), []byte(`"id":"fast"`)) {
		t.Fatal("wait blocked another request")
	}
	p.send(nil, "notifications/cancelled", map[string]any{"requestId": 7})
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("matching HTTP request not cancelled")
	}
	p.send("fence", "ping", nil)
	if !bytes.Contains(p.frame(), []byte(`"id":"fence"`)) {
		t.Fatal("cancelled request produced a response or cancelled unrelated work")
	}
}

func TestBridgeCancelAndLostReply(t *testing.T) {
	var calls atomic.Int32
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method == "tools/call" {
			if calls.Add(1) > 1 {
				return nil, errors.New("unexpected mutation resend")
			}
			return nil, io.EOF
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool("lost", "repo_archive", map[string]any{"repo": "upstream/org/app"}, nil)
	data := p.frame()
	if calls.Load() != 1 || !bytes.Contains(data, []byte("Reply lost; nothing was retried.")) || !bytes.Contains(data, []byte("Read the object's state")) {
		t.Fatalf("lost mutation was retried or unclear: %s (%d)", data, calls.Load())
	}
}

func TestBridgeProgressFromKeepalive(t *testing.T) {
	for _, token := range []any{nil, "progress-one", int64(42)} {
		t.Run(strings.TrimSpace(string(mustJSON(token))), func(t *testing.T) {
			release := make(chan struct{})
			b := fake.Bindings()
			b.RunWait.Handler = &fake.Handler[apicontract.RunWaitArgs, apicontract.WaitResult]{Run: func(ctx context.Context) (apicontract.WaitResult, error) {
				select {
				case <-ctx.Done():
					return apicontract.WaitResult{}, ctx.Err()
				case <-release:
					return apicontract.WaitResult{}, nil
				}
			}}
			p := newBridgePeer(t, bridgeProfile(t, streamingTransport(server(t, b))))
			p.initialize()
			p.tool("wait", "run_wait", map[string]any{"id": "run1", "timeout_s": 600}, token)
			if token != nil {
				data := p.frame()
				var notice struct {
					Method string
					Params struct {
						ProgressToken json.RawMessage `json:"progressToken"`
						Progress      int
					}
				}
				if json.Unmarshal(data, &notice) != nil || notice.Method != "notifications/progress" || notice.Params.Progress != 1 || string(notice.Params.ProgressToken) != string(mustJSON(token)) {
					t.Fatalf("progress missing or altered: %s", data)
				}
			}
			close(release)
			if !bytes.Contains(p.frame(), []byte(`"id":"wait"`)) {
				t.Fatal("progress emitted without a token or final response lost")
			}
		})
	}
}

func mustJSON(v any) []byte { data, _ := json.Marshal(v); return data }
