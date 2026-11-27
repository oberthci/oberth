package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"

	"github.com/oberthci/oberth/internal/apicontract"
)

func modernRequest(r *http.Request) *http.Request {
	if r.Method != "POST" || (r.URL.Path != "/mcp" && r.URL.Path != "/mcp/enroll") {
		return r
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return r
	}
	originalSize := len(raw)
	var wire apicontract.MCPRequest
	if json.Unmarshal(raw, &wire) == nil {
		params, err := apicontract.WithClientMetadata(wire.Params)
		if err == nil {
			wire.Params = params
			raw, _ = json.Marshal(wire)
			if len(raw) < originalSize {
				raw = append(raw, bytes.Repeat([]byte(" "), originalSize-len(raw))...)
			}
			for name, values := range wire.Headers() {
				r.Header[name] = values
			}
		}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return r
}
