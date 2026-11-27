package api_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func wireRequest(method string, params any) *http.Request {
	raw, _ := json.Marshal(params)
	if params == nil {
		raw = nil
	}
	raw, _ = apicontract.WithClientMetadata(raw)
	wire := apicontract.MCPRequest{JSONRPC: "2.0", ID: json.RawMessage(`"wire"`), Method: method, Params: raw}
	body, _ := json.Marshal(wire)
	r := httptest.NewRequest("POST", "/mcp", bytes.NewReader(body))
	r.Header = wire.Headers()
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer valid")
	return r
}

func assertRPCCode(t *testing.T, w *httptest.ResponseRecorder, status, code int) {
	t.Helper()
	var reply struct {
		ID    string
		Error struct{ Code int }
	}
	if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != status || reply.Error.Code != code || reply.ID != "wire" {
		t.Fatalf("HTTP %d: %s; want %d/%d", w.Code, w.Body.String(), status, code)
	}
}

func TestMCP20260728Wire(t *testing.T) {
	f := setup(t, nil)
	for _, method := range []string{"server/discover", "ping", "tools/list", "resources/list", "tools/call", "resources/read"} {
		var params any
		switch method {
		case "tools/call":
			params = map[string]any{"name": "system_status", "arguments": map[string]any{}}
		case "resources/read":
			params = map[string]any{"uri": "run://12345678"}
		}
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, wireRequest(method, params))
		var reply struct{ Error json.RawMessage }
		if json.Unmarshal(w.Body.Bytes(), &reply) != nil || w.Code != 200 || reply.Error != nil {
			t.Fatal("modern method failed", method, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"resultType":"complete"`) {
			t.Fatal("missing discriminator", method)
		}
		if w.Header().Get("Mcp-Session-Id") != "" {
			t.Fatal("minted legacy session")
		}
	}
	checkMCPHeaders(t, f)
	checkMCPMethods(t, f)
}

func checkMCPHeaders(t *testing.T, f *fixture) {
	t.Helper()
	for _, header := range []string{"MCP-Protocol-Version", "Mcp-Method", "Mcp-Name"} {
		for _, mode := range []string{"missing", "mismatch", "duplicate"} {
			r := wireRequest("tools/call", map[string]any{"name": "run_rerun", "arguments": map[string]string{"id": "12345678"}})
			switch mode {
			case "missing":
				r.Header.Del(header)
			case "mismatch":
				r.Header.Set(header, "other")
			case "duplicate":
				r.Header.Add(header, r.Header.Get(header))
			}
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, r)
			assertRPCCode(t, w, 400, -32020)
		}
	}
	h := f.bindings.RunRerun.Handler.(*fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt])
	if inputs, _, calls := h.Observed(); len(inputs) != 0 || calls != 0 {
		t.Fatal("bad headers reached preparation")
	}
}

func checkMCPMethods(t *testing.T, f *fixture) {
	t.Helper()
	for _, method := range []string{"initialize", "tasks/get", "resources/subscribe", "unknown"} {
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, wireRequest(method, nil))
		assertRPCCode(t, w, 404, -32601)
	}
	for _, method := range []string{"GET", "DELETE"} {
		w := request(f, method, "/mcp", nil)
		if w.Code != 405 {
			t.Fatal("legacy endpoint remains", method, w.Code)
		}
	}
}

func TestMCPEncodedHeaders(t *testing.T) {
	f := setup(t, nil)
	for _, value := range []string{"system_status", "日本語", " padded ", "line\nvalue", "=?base64?literal?="} {
		r := wireRequest("tools/call", map[string]any{"name": value})
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		if value == "system_status" {
			assertSuccess(t, w)
		} else {
			assertRPCCode(t, w, 400, -32602)
		}
	}
	for _, value := range []string{"=?base64?@@?=", "=?base64?c3lzdGVtX3N0YXR1cw?=", "=?base64?/w==?=", "other", "system_status "} {
		r := wireRequest("tools/call", map[string]string{"name": "system_status"})
		r.Header.Set("Mcp-Name", value)
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		assertRPCCode(t, w, 400, -32020)
	}
	r := wireRequest("tools/call", map[string]string{"name": "system_status"})
	r.Header.Set("Mcp-Name", "=?base64?c3lzdGVtX3N0YXR1cw==?=")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	assertSuccess(t, w)
}

func TestMCPVersionsAndEnrollmentBeforeEffects(t *testing.T) {
	for _, path := range []string{"/mcp", "/mcp/enroll"} {
		for _, version := range []string{"2025-03-26", "2025-06-18", "2025-11-25", "2030-01-01"} {
			f := setup(t, nil)
			r := wireRequest("ping", nil)
			var wire apicontract.MCPRequest
			_ = json.NewDecoder(r.Body).Decode(&wire)
			p := wire.Metadata()
			p.Meta[apicontract.ProtocolVersionMeta], _ = json.Marshal(version)
			wire.Params, _ = json.Marshal(p)
			body, _ := json.Marshal(wire)
			r.Body = io.NopCloser(bytes.NewReader(body))
			r.Header.Set("MCP-Protocol-Version", version)
			r.URL.Path = path
			if path == "/mcp/enroll" {
				r.Header.Del("Authorization")
			}
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, r)
			assertRPCCode(t, w, 400, -32022)
			if !strings.Contains(w.Body.String(), `"supported":["2026-07-28"]`) {
				t.Fatal("no version remedy")
			}
		}
		f := setup(t, nil)
		r := wireRequest("tools/call", map[string]any{"name": "uplink_redeem", "arguments": map[string]string{"secret": "private"}})
		r.URL.Path = path
		if path == "/mcp/enroll" {
			r.Header.Del("Authorization")
		}
		r.Header.Del("Mcp-Name")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		assertRPCCode(t, w, 400, -32020)
	}
}

func TestMCPMetadataAndEnrollmentRejectBeforePreparation(t *testing.T) {
	h := &fake.Handler[model.InviteRedemption, model.InviteCertificate]{}
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Enroll = h })
	for _, invalid := range []json.RawMessage{nil, json.RawMessage(`null`), json.RawMessage(`[]`), json.RawMessage(`"bad"`)} {
		r := wireRequest("tools/call", map[string]any{"name": "uplink_redeem", "arguments": map[string]string{"secret": "private", "csr": "csr", "server_url": "https://example.org/mcp", "ca_pin": "pin", "ssh_public_key": "public"}})
		var wire apicontract.MCPRequest
		_ = json.NewDecoder(r.Body).Decode(&wire)
		var params map[string]json.RawMessage
		_ = json.Unmarshal(wire.Params, &params)
		meta := wire.Metadata().Meta
		if invalid == nil {
			delete(meta, apicontract.ClientCapabilitiesMeta)
		} else {
			meta[apicontract.ClientCapabilitiesMeta] = invalid
		}
		params["_meta"], _ = json.Marshal(meta)
		wire.Params, _ = json.Marshal(params)
		body, _ := json.Marshal(wire)
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.URL.Path = "/mcp/enroll"
		r.Header.Del("Authorization")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		assertRPCCode(t, w, 400, -32602)
	}
	if inputs, _, calls := h.Observed(); len(inputs) != 0 || calls != 0 {
		t.Fatal("invalid metadata redeemed invitation")
	}
}

func TestMCPRejectsBrowserOriginsAndEnrollmentMedia(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/mcp", "/mcp/enroll"} {
		r := httptest.NewRequest("POST", path, nil)
		r.Body = unreadBody{t}
		r.Header.Set("Origin", "https://untrusted.example")
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatal("browser origin reached MCP", path, w.Code)
		}
	}
	r := wireRequest("ping", nil)
	r.URL.Path = "/mcp/enroll"
	r.Header.Del("Authorization")
	r.Header.Set("Content-Type", "text/plain")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("non-JSON enrollment accepted")
	}
}

func TestMCPRejectsMalformedContinuationEnvelopeBeforeEffects(t *testing.T) {
	f := setup(t, nil)
	for _, fields := range []map[string]any{{"requestState": true}, {"inputResponses": []string{"answer"}}} {
		fields["name"] = "run_rerun"
		fields["arguments"] = map[string]string{"id": "11111111"}
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, wireRequest("tools/call", fields))
		assertRPCCode(t, w, 400, -32602)
	}
	h := f.bindings.RunRerun.Handler.(*fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt])
	if inputs, _, calls := h.Observed(); len(inputs) != 0 || calls != 0 {
		t.Fatal("malformed continuation reached mutation")
	}
}
