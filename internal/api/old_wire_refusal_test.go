package api_test

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestWalk18InitializeHasReadableProtocolRefusal(t *testing.T) {
	f := setup(t, nil)
	// The recorded bridge request has ordinary host fields and no new metadata.
	wire := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"walk18","version":"v0.30.18"}}}`
	for _, version := range []string{"", "2025-06-18"} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(wire))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer valid")
		if version != "" {
			r.Header.Set("MCP-Protocol-Version", version)
		}
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, r)
		// The installed bridge's non-200 branch calls resultError on this body.
		var refusal struct {
			Code           publicerr.Code
			Reason, Remedy string
		}
		if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil || w.Code != 400 || !refusal.Code.Valid() {
			t.Fatal("old bridge cannot decode refusal", w.Code, w.Body, err)
		}
		if refusal.Reason != "MCP 2026-07-28 is required." || !strings.Contains(refusal.Remedy, "client matching the server release") {
			t.Fatal("missing floor or matching-client remedy", refusal)
		}
		if publicerr.CheckMessage(refusal.Reason, 80) != nil || publicerr.CheckMessage(refusal.Remedy, 120) != nil {
			t.Fatal("refusal exceeds message bounds")
		}
	}
}
