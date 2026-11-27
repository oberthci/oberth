package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
)

// Browser callers use the configured public origin, never an origin inferred
// from the request's untrusted Host header. The CLI omits Origin.
func (s *Server) mcpOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origins := r.Header.Values("Origin")
		if len(origins) != 0 && (len(origins) != 1 || s.config.BrowserOrigin == "" || origins[0] != s.config.BrowserOrigin) {
			s.writeError(w, r, refusal("origin"), http.StatusForbidden)
			return
		}
		next(w, r)
	}
}

func browserOrigin(address string) (string, error) {
	if address == "" {
		return "", nil
	}
	u, err := url.Parse(address)
	if err != nil {
		return "", refusal("originConfig")
	}
	scheme := strings.ToLower(u.Scheme)
	if !validOriginURL(u, scheme) {
		return "", refusal("originConfig")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if port := originPort(scheme, u.Port()); port != "" {
		host += ":" + port
	}
	return scheme + "://" + host, nil
}

func validOriginURL(u *url.URL, scheme string) bool {
	return (scheme == "https" || scheme == "http") && u.Hostname() != "" && u.User == nil && u.Opaque == "" && u.RawQuery == "" && u.Fragment == ""
}

func originPort(scheme, port string) string {
	if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
		return ""
	}
	return port
}

// MarshalJSON covers both ordinary replies and request-scoped SSE final frames.
func (resp rpcResponse) MarshalJSON() ([]byte, error) {
	type plain rpcResponse
	if resp.Error != nil || resp.Result == nil {
		return json.Marshal(plain(resp))
	}
	raw, err := json.Marshal(resp.Result)
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, err
	}
	if result == nil {
		result = map[string]json.RawMessage{}
	}
	meta := map[string]json.RawMessage{}
	if raw := result["_meta"]; raw != nil {
		if err := json.Unmarshal(raw, &meta); err != nil {
			return nil, err
		}
	}
	if meta == nil {
		meta = map[string]json.RawMessage{}
	}
	meta["io.modelcontextprotocol/serverInfo"], _ = json.Marshal(map[string]string{"name": "Oberth", "version": resp.serverVersion})
	result["_meta"], _ = json.Marshal(meta)
	result["resultType"] = json.RawMessage(`"complete"`)
	// Cacheable discovery, catalogues and reads all use immediate rediscovery.
	// Tool results are not cacheable; these optional fields are harmless there.
	if _, ok := result["ttlMs"]; !ok {
		result["ttlMs"] = json.RawMessage(`0`)
	}
	if _, ok := result["cacheScope"]; !ok {
		result["cacheScope"] = json.RawMessage(`"private"`)
	}
	resp.Result = result
	return json.Marshal(plain(resp))
}

func (s *Server) validateMCP(w http.ResponseWriter, r *http.Request, req rpcRequest) bool {
	// Clients without per-request metadata decode HTTP refusals as public errors.
	// Refuse before dispatch, in the envelope they can actually display.
	if req.Version() == "" && r.Header.Get("Mcp-Method") == "" {
		s.writeError(w, r, refusal("version"), http.StatusBadRequest)
		return false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	if len(req.ID) == 0 {
		resp.ID = json.RawMessage(`null`)
	}
	if !req.MatchingHeaders(r.Header) {
		resp.Error = &rpcError{-32020, refusal("headers").Reason(), refusal("headers")}
	} else if version := req.Version(); version != apicontract.ProtocolVersion {
		problem := refusal("version")
		resp.Error = &rpcError{-32022, problem.Reason(), map[string]any{
			"supported": []string{apicontract.ProtocolVersion}, "requested": version,
			"reason": problem.Reason(), "remedy": problem.Remedy(),
		}}
	} else if !req.ValidMetadata() {
		resp.Error = &rpcError{-32602, refusal("metadata").Reason(), refusal("metadata")}
	}
	if resp.Error == nil {
		return true
	}
	s.writeRPC(w, r, resp)
	return false
}
