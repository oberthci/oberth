package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestInitializeNegotiatesSupportedOrFallbackVersion(t *testing.T) {
	for _, version := range []string{mcpProtocolVersion, "2099-01-01"} {
		s, _ := testServer(t)
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`, version)))
		req.Header.Set("Authorization", "Bearer valid-token")
		out := httptest.NewRecorder()
		s.Handler().ServeHTTP(out, req)
		var response struct {
			Result struct {
				Version string `json:"protocolVersion"`
			}
		}
		if err := json.Unmarshal(out.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if response.Result.Version != mcpProtocolVersion {
			t.Fatalf("negotiated %q", response.Result.Version)
		}
	}
}
