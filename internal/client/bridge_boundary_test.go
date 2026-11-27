package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestBridgeInputAndOutputBoundaries(t *testing.T) {
	for _, wire := range []string{"not json\n", `{"jsonrpc":"2.0","id":null,"method":"ping"}` + "\n", string([]byte{'{', '"', 'x', '"', ':', '"', 255, '"', '}', '\n'})} {
		var out bytes.Buffer
		if err := Bridge(t.Context(), strings.NewReader(wire), &out, bridgeProfile(t, handlerTransport(server(t, fake.Bindings())))); err != nil {
			t.Fatal(err)
		}
		var reply struct {
			ID    any
			Error any
		}
		if json.Unmarshal(bytes.TrimSpace(out.Bytes()), &reply) != nil || reply.ID != nil || reply.Error == nil {
			t.Fatal("invalid stdio frame accepted")
		}
	}
	if err := Bridge(t.Context(), strings.NewReader(strings.Repeat("x", apicontract.RequestBytes+1)+"\n"), io.Discard, bridgeProfile(t, handlerTransport(server(t, fake.Bindings())))); err == nil {
		t.Fatal("unbounded input")
	}
	if err := Bridge(t.Context(), strings.NewReader("not json\n"), shortBridgeOutput{}, bridgeProfile(t, handlerTransport(server(t, fake.Bindings())))); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short output write accepted")
	}
}

type shortBridgeOutput struct{}

func (shortBridgeOutput) Write([]byte) (int, error) { return 0, nil }

func TestBridgeIDsAndConcurrentWrites(t *testing.T) {
	p := wrappedBridgePeer(t, bridgeProfile(t, handlerTransport(server(t, fake.Bindings()))), func(out io.WriteCloser) io.WriteCloser { return fragmentedOutput{out} })
	p.discover()
	for i := 0; i < 20; i++ {
		p.send(i, "ping", nil)
	}
	seen := map[int]bool{}
	for range 20 {
		var reply struct {
			ID    int
			Error any
		}
		if json.Unmarshal(p.frame(), &reply) != nil || reply.Error != nil || seen[reply.ID] {
			t.Fatal("interleaved or duplicate response")
		}
		seen[reply.ID] = true
	}
	if len(seen) != 20 {
		t.Fatal("response lost")
	}
}

type fragmentedOutput struct{ io.WriteCloser }

func (f fragmentedOutput) Write(data []byte) (int, error) {
	for i := range data {
		if _, err := f.WriteCloser.Write(data[i : i+1]); err != nil {
			return i, err
		}
		runtime.Gosched()
	}
	return len(data), nil
}

func TestBridgeForwardsOnlyBearerDigest(t *testing.T) {
	b := fake.Bindings()
	h := &fake.Handler[apicontract.UplinkAddArgs, apicontract.Identity]{Value: apicontract.Identity{Identity: "agent"}}
	b.UplinkAdd.Handler = h
	cfg := bridgeProfile(t, handlerTransport(server(t, b)))
	p := newBridgePeer(t, cfg)
	p.discover()
	digest := apicontract.SecretDigest("caller-retained-bearer")
	p.tool("private", "uplink_add", map[string]any{"identity": "agent", "ssh_public_key": "public-key", "bearer_digest": digest}, nil)
	data := p.frame()
	inputs, _, calls := h.Observed()
	if calls != 1 || inputs[0].BearerDigest != digest || bytes.Contains(data, []byte("token")) {
		t.Fatalf("bearer protocol changed: %s", data)
	}
	if _, err := os.Stat(filepath.Join(cfg.Base, "credentials")); !os.IsNotExist(err) {
		t.Fatal("digest-only reply created local credentials", err)
	}
}

func TestBridgeDuplicateIDDoesNotCancelOriginal(t *testing.T) {
	entered := make(chan struct{})
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method != "tools/call" {
			return handlerTransport(h).RoundTrip(r)
		}
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	p := newBridgePeer(t, cfg)
	p.discover()
	p.tool(4, "run_get", map[string]any{"id": "run1"}, nil)
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("request never started")
	}
	p.send(4, "ping", nil)
	if !bytes.Contains(p.frame(), []byte("request ID is active")) {
		t.Fatal("duplicate RPC ID replaced original request")
	}
	p.send(nil, "notifications/cancelled", map[string]any{"requestId": 4})
}

func TestBridgeBoundsActiveRequests(t *testing.T) {
	h := server(t, fake.Bindings())
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		req := readBridgeRequest(r)
		if req.Method != "tools/call" {
			return handlerTransport(h).RoundTrip(r)
		}
		<-r.Context().Done()
		return nil, r.Context().Err()
	}))
	p := newBridgePeer(t, cfg)
	p.discover()
	for id := 0; id < 65; id++ {
		p.tool(id, "run_get", map[string]any{"id": "run1"}, nil)
	}
	if data := p.frame(); !bytes.Contains(data, []byte("too many requests")) {
		t.Fatal("active requests were not bounded")
	}
}
