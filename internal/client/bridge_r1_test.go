package client

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBridgeCompactsMultilineReply(t *testing.T) {
	var out bytes.Buffer
	b := bridge{ctx: t.Context(), out: &out, cancel: func() {}}
	if err := b.emit(t.Context(), []byte("{\n \"jsonrpc\":\"2.0\",\n \"id\":1,\"result\":{}\n}")); err != nil {
		t.Fatal(err)
	}
	if strings.Count(out.String(), "\n") != 1 || !json.Valid(bytes.TrimSpace(out.Bytes())) {
		t.Fatal("multiline protocol frame")
	}
}
func TestBridgeRejectsMismatchedRelayID(t *testing.T) {
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method == "ping" {
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"jsonrpc":"2.0","id":"foreign","result":{}}`))}, nil
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send("expected", "ping", nil)
	var reply struct {
		ID    string
		Error any
	}
	if json.Unmarshal(p.frame(), &reply) != nil || reply.ID != "expected" || reply.Error == nil {
		t.Fatal("foreign reply relayed")
	}
}
func TestBridgeCleansRefusedCredentialReservations(t *testing.T) {
	cfg := bridgeProfile(t, handlerTransport(server(t, fake.Bindings())))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool(1, "uplink_add", map[string]any{}, nil)
	p.frame()
	_ = p.input.Close()
	<-p.done
	entries, err := os.ReadDir(filepath.Join(cfg.Base, "credentials"))
	if !os.IsNotExist(err) && err != nil || len(entries) != 0 {
		t.Fatalf("unused credential reservation: %v %v", entries, err)
	}
}
func TestBridgeEOFDrainsAcceptedMutation(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	b := fake.Bindings()
	b.RunRerun.Handler = &fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt]{Run: func(ctx context.Context) (apicontract.Receipt, error) {
		close(entered)
		select {
		case <-release:
			return apicontract.Receipt{ID: "rerun"}, nil
		case <-ctx.Done():
			return apicontract.Receipt{}, ctx.Err()
		}
	}}
	stream := make(chan context.Context, 1)
	h := server(t, b)
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			return handlerTransport(h).RoundTrip(r)
		}
		stream <- r.Context()
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(untilCancelled{r.Context()})}, nil
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.send(nil, "notifications/initialized", nil)
	streaming := awaitEventContext(t, stream)
	p.tool("mutation", "run_rerun", map[string]any{"id": "run1"}, nil)
	<-entered
	_ = p.input.Close()
	awaitEventSignal(t, streaming.Done())
	timer := time.NewTimer(30 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-p.done:
		t.Fatalf("EOF cancelled an accepted request: %v", err)
	case <-timer.C:
	}
	close(release)
	frame := p.frame()
	var reply struct {
		ID    string
		Error any
	}
	if json.Unmarshal(frame, &reply) != nil || reply.ID != "mutation" || reply.Error != nil {
		t.Fatalf("accepted mutation lost: %s", frame)
	}
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("drained bridge did not exit")
	}
}
func TestBridgeCancelledMutationReportsLostReply(t *testing.T) {
	entered := make(chan struct{})
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if readBridgeRequest(r).Method == "tools/call" {
			close(entered)
			<-r.Context().Done()
			return nil, r.Context().Err()
		}
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.initialize()
	p.tool("mut", "run_rerun", map[string]any{"id": "run1"}, nil)
	<-entered
	p.send(nil, "notifications/cancelled", map[string]any{"requestId": "mut"})
	if !bytes.Contains(p.frame(), []byte("Reply lost; nothing was retried")) {
		t.Fatal("cancelled mutation silently dropped")
	}
}
