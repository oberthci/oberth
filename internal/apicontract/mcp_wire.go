package apicontract

// Minimal shared wire types follow the MCP 2026-07-28 schema. Request metadata
// is declarative client information; it never replaces authenticated authority.
import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"unicode/utf8"
)

const (
	ProtocolVersion        = "2026-07-28"
	ProtocolVersionMeta    = "io.modelcontextprotocol/protocolVersion"
	ClientInfoMeta         = "io.modelcontextprotocol/clientInfo"
	ClientCapabilitiesMeta = "io.modelcontextprotocol/clientCapabilities"
)

type MCPRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// MCPParams preserves the MRTR envelope without treating it as tool arguments.
type MCPParams struct {
	Meta           map[string]json.RawMessage `json:"_meta,omitempty"`
	RequestState   string                     `json:"requestState,omitempty"`
	InputResponses map[string]json.RawMessage `json:"inputResponses,omitempty"`
}

func ClientMetadata() map[string]json.RawMessage {
	return map[string]json.RawMessage{
		ProtocolVersionMeta:    json.RawMessage(`"` + ProtocolVersion + `"`),
		ClientInfoMeta:         json.RawMessage(`{"name":"oberth","version":"1"}`),
		ClientCapabilitiesMeta: json.RawMessage(`{}`),
	}
}

// WithClientMetadata is only for requests originated by the standalone CLI.
// A bridge forwards the host's metadata unchanged, including unknown fields.
func WithClientMetadata(raw json.RawMessage) (json.RawMessage, error) {
	var params map[string]json.RawMessage
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &params); err != nil {
			return nil, err
		}
	}
	if params == nil {
		params = map[string]json.RawMessage{}
	}
	meta, _ := json.Marshal(ClientMetadata())
	params["_meta"] = meta
	return json.Marshal(params)
}

func (r MCPRequest) Metadata() MCPParams {
	var p MCPParams
	_ = json.Unmarshal(r.Params, &p)
	return p
}

func (r MCPRequest) Version() string {
	var version string
	_ = json.Unmarshal(r.Metadata().Meta[ProtocolVersionMeta], &version)
	return version
}

func (r MCPRequest) ValidMetadata() bool {
	var params MCPParams
	if json.Unmarshal(r.Params, &params) != nil {
		return false
	}
	meta := params.Meta
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(meta[ClientCapabilitiesMeta], &capabilities) != nil || capabilities == nil {
		return false
	}
	if raw, present := meta[ClientInfoMeta]; present {
		var info struct{ Name, Version string }
		if json.Unmarshal(raw, &info) != nil || info.Name == "" || info.Version == "" {
			return false
		}
	}
	return true
}

func (r MCPRequest) RoutingName() (string, bool) {
	field := ""
	switch r.Method {
	case "tools/call", "prompts/get":
		field = "name"
	case "resources/read":
		field = "uri"
	default:
		return "", false
	}
	var params map[string]json.RawMessage
	var name string
	_ = json.Unmarshal(r.Params, &params)
	_ = json.Unmarshal(params[field], &name)
	return name, true
}

func (r MCPRequest) Headers() http.Header {
	h := http.Header{}
	h.Set("MCP-Protocol-Version", r.Version())
	h.Set("Mcp-Method", r.Method)
	if name, needed := r.RoutingName(); needed {
		h.Set("Mcp-Name", EncodeMCPHeader(name))
	}
	return h
}

func sentinel(value string) bool {
	return strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=")
}

func plainMCPHeader(value string) bool {
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, c := range value {
		if c != '\t' && (c < 0x20 || c > 0x7e) {
			return false
		}
	}
	return true
}

func EncodeMCPHeader(value string) string {
	if plainMCPHeader(value) && !sentinel(value) {
		return value
	}
	return "=?base64?" + base64.StdEncoding.EncodeToString([]byte(value)) + "?="
}

func DecodeMCPHeader(value string) (string, bool) {
	if !plainMCPHeader(value) {
		return "", false
	}
	if !sentinel(value) {
		return value, true
	}
	raw, err := base64.StdEncoding.Strict().DecodeString(value[len("=?base64?") : len(value)-2])
	return string(raw), err == nil && utf8.Valid(raw)
}

func (r MCPRequest) MatchingHeaders(h http.Header) bool {
	for name, want := range map[string]string{"MCP-Protocol-Version": r.Version(), "Mcp-Method": r.Method} {
		values := h.Values(name)
		if want == "" || !plainMCPHeader(want) || len(values) != 1 || values[0] != want {
			return false
		}
	}
	if name, needed := r.RoutingName(); needed {
		values := h.Values("Mcp-Name")
		if len(values) != 1 {
			return false
		}
		decoded, valid := DecodeMCPHeader(values[0])
		if !valid || decoded != name {
			return false
		}
	}
	return true
}
