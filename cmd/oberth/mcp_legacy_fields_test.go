package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oberthci/oberth/internal/api"
)

func TestMCPLegacyFieldsOptIn(t *testing.T) {
	t.Setenv("OBERTH_MCP_URL", "https://example.invalid/mcp")
	t.Setenv("OBERTH_TOKEN", "legacy-test-token")
	for _, test := range []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{"default", "", nil, false},
		{"flag", "", []string{"--legacy-fields"}, true},
		{"environment", "1", nil, true},
		{"flag overrides environment", "1", []string{"--legacy-fields=false"}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("OBERTH_LEGACY_FIELDS", test.env)
			cfg, err := parseMCPFlags("mcp call", test.args, 0)
			if err != nil || cfg.legacyFields != test.want {
				t.Fatalf("legacyFields = %v, %v; want %v", cfg.legacyFields, err, test.want)
			}
			var seen string
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen = r.Header.Get(api.LegacyFieldsHeader)
				id := 1
				testWriteJSONRPCError(w, &id, -32601, "method not found")
			}))
			defer server.Close()
			defer withTestMCPClient(t, server)()
			cfg.URL = server.URL
			if _, err := mcpRequest(context.Background(), cfg, mcpMethodToolsList, nil); err != nil {
				t.Fatal(err)
			}
			if (seen == "1") != test.want {
				t.Fatalf("header %q sent, want %v", seen, test.want)
			}
		})
	}
}
