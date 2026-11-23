package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestPublicationOrderSurvivesCLIJSONAndMCP(t *testing.T) {
	b := fake.Bindings()
	b.PublicationList.Handler = &fake.Handler[apicontract.PublicationListArgs, apicontract.PublicationPage]{Value: apicontract.PublicationPage{
		Publications: []model.Publication{{ID: "a-new"}, {ID: "z-old"}}, NextCursor: "z-old"}}
	a, out, diag := fixture(t, b)
	for _, options := range [][]string{nil, {"-o", "json"}} {
		out.Reset()
		if a.run(t.Context(), append([]string{"publication", "list"}, options...)) != 0 {
			t.Fatal(diag.String())
		}
		assertPublicationOrder(t, out.String())
	}
	a, _ = boundMCPApplication(t, b)
	encoder, decoder := mcpPeer(t, a, []string{"mcp", "--profile", "pinned"})
	var reply json.RawMessage
	for _, request := range []map[string]any{
		{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-06-18"}},
		{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "publication_list", "arguments": map[string]any{}}},
	} {
		if err := encoder.Encode(request); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode(&reply); err != nil {
			t.Fatal(err)
		}
	}
	assertPublicationOrder(t, string(reply))
}

func assertPublicationOrder(t *testing.T, text string) {
	t.Helper()
	newLabel, oldLabel := "Run a-new", "Run z-old"
	if !strings.Contains(text, newLabel) {
		text = strings.Join(strings.Fields(strings.ReplaceAll(text, `\"`, `"`)), "")
		newLabel, oldLabel = `"id":"a-new"`, `"id":"z-old"`
	}
	newest, oldest := strings.Index(text, newLabel), strings.Index(text, oldLabel)
	if newest < 0 || oldest < newest {
		t.Fatalf("publication chronology changed: %s", text)
	}
}
