package client

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestMCP20260728Bridge(t *testing.T) {
	h := server(t, fake.Bindings())
	seen := make(chan rpcRequest, 8)
	var legacy atomic.Int32
	cfg := bridgeProfile(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		if r.Method != "POST" || r.Header.Get("Mcp-Session-Id") != "" || r.Header.Get("Last-Event-ID") != "" {
			legacy.Add(1)
		}
		wire := readBridgeRequest(r)
		if !wire.MatchingHeaders(r.Header) {
			t.Error("bridge headers differ from body")
		}
		seen <- wire
		return handlerTransport(h).RoundTrip(r)
	}))
	p := newBridgePeer(t, cfg)
	p.discover()
	<-seen
	params := map[string]any{"name": "run_get", "arguments": map[string]string{"id": "12345678"},
		"requestState": "opaque", "inputResponses": map[string]any{"answer": map[string]any{"action": "decline"}},
		"_meta": map[string]any{"progressToken": "p", "example.org/opaque": map[string]any{"data": "unchanged"}, apicontract.ClientInfoMeta: map[string]string{"name": "host", "version": "7"}}}
	p.send("preserved", "tools/call", params)
	data := p.frame()
	if _, refused, _, err := apicontract.ToolPayloadForID(data, json.RawMessage(`"preserved"`)); err != nil || refused {
		t.Fatal(string(data), err)
	}
	wire := <-seen
	checkForwardedParams(t, wire, params)
	checkBridgeProtocolErrors(t, p, seen)
	_ = p.input.Close()
	if err := <-p.done; err != nil {
		t.Fatal(err)
	}
	if legacy.Load() != 0 {
		t.Fatal("legacy transport used")
	}
}

func checkForwardedParams(t *testing.T, wire rpcRequest, params map[string]any) {
	t.Helper()
	var got map[string]any
	_ = json.Unmarshal(wire.Params, &got)
	for _, field := range []string{"requestState", "inputResponses"} {
		if !reflect.DeepEqual(got[field], params[field]) {
			t.Fatal("MRTR field changed", field)
		}
	}
	meta := got["_meta"].(map[string]any)
	if !reflect.DeepEqual(meta["example.org/opaque"], params["_meta"].(map[string]any)["example.org/opaque"]) || meta[apicontract.ClientInfoMeta].(map[string]any)["name"] != "host" {
		t.Fatal("host metadata changed")
	}
}

func checkBridgeProtocolErrors(t *testing.T, p *bridgePeer, seen <-chan rpcRequest) {
	t.Helper()
	for _, tc := range []struct {
		method, version string
		code            int
	}{{"unknown", apicontract.ProtocolVersion, -32601}, {"ping", "2025-06-18", -32022}, {"initialize", apicontract.ProtocolVersion, -32601}} {
		p.send("error-"+tc.method, tc.method, map[string]any{"_meta": map[string]any{apicontract.ProtocolVersionMeta: tc.version}})
		var reply struct{ Error struct{ Code int } }
		if json.Unmarshal(p.frame(), &reply) != nil || reply.Error.Code != tc.code {
			t.Fatal("bridge changed protocol error", reply.Error.Code, tc.code)
		}
		<-seen
	}
}

func TestMCP20260728StandaloneMetadata(t *testing.T) {
	h := server(t, fake.Bindings())
	c := testClient(t, roundTrip(func(r *http.Request) (*http.Response, error) {
		wire := readBridgeRequest(r)
		p := wire.Metadata()
		if !wire.MatchingHeaders(r.Header) || wire.Version() != apicontract.ProtocolVersion || p.Meta[apicontract.ClientInfoMeta] == nil || string(p.Meta[apicontract.ClientCapabilitiesMeta]) != "{}" {
			t.Fatal("ordinary CLI omitted metadata")
		}
		return handlerTransport(h).RoundTrip(r)
	}), nil)
	if err := c.Invoke(t.Context(), "repo_list", apicontract.RepoListArgs{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestMCPInputRequiredPassesBridgeAndStopsCLI(t *testing.T) {
	h := server(t, fake.Bindings())
	var calls atomic.Int32
	rt := roundTrip(func(r *http.Request) (*http.Response, error) {
		wire := readBridgeRequest(r)
		if wire.Method != "tools/call" {
			return handlerTransport(h).RoundTrip(r)
		}
		calls.Add(1)
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": wire.ID, "result": map[string]any{"resultType": "input_required", "requestState": "opaque-state", "_meta": map[string]string{"example.org/state": "unchanged"}}})
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(data)), Request: r}, nil
	})
	p := newBridgePeer(t, bridgeProfile(t, rt))
	p.discover()
	p.tool("continuation", "run_rerun", map[string]string{"id": "11111111"}, nil)
	data := p.frame()
	if !bytes.Contains(data, []byte(`"resultType":"input_required"`)) || !bytes.Contains(data, []byte(`"requestState":"opaque-state"`)) || !bytes.Contains(data, []byte(`"example.org/state":"unchanged"`)) || calls.Load() != 1 {
		t.Fatal("bridge consumed or replayed MRTR result", string(data))
	}
	calls.Store(0)
	c := testClient(t, rt, nil)
	err := c.Invoke(t.Context(), "run_rerun", apicontract.RunRerunArgs{ID: "11111111"}, nil)
	if err == nil || !strings.Contains(err.Error(), "interactive host") || calls.Load() != 1 {
		t.Fatal("CLI accepted interaction or replayed action", calls.Load(), err)
	}
}
