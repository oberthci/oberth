package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConcurrencyGroupsCLIIsReadOnly(t *testing.T) {
	const token = "concurrency-test-token"
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			t.Error("wrong credential binding")
		}
		var request jsonRPCRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		body, err := json.Marshal(request.Params)
		if err != nil {
			t.Error(err)
			return
		}
		var params toolsCallParams
		if err := json.Unmarshal(body, &params); err != nil {
			t.Error(err)
			return
		}
		if request.Method != mcpMethodToolsCall || params.Name != "concurrency_groups" || string(params.Arguments) != "{}" {
			t.Errorf("unexpected request: %+v %+v", request, params)
		}
		testWriteJSONRPCResult(w, &request.ID, toolsCallResult{Content: []mcpContent{{Type: "text", Text: `{"groups":[]}`}}})
	}))
	defer server.Close()
	defer withTestMCPClient(t, server)()
	t.Setenv("OBERTH_TOKEN", token)
	t.Setenv("OBERTH_MCP_URL", server.URL)
	var output strings.Builder
	if err := runAdmission(t.Context(), []string{"groups", "--json"}, &output); err != nil || output.String() != "{\"groups\":[]}\n" {
		t.Fatal(output.String(), err)
	}
}
