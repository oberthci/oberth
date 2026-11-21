package api_test

import (
	"encoding/json"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
)

// Catalogue tests consume the cached public wire listing. Private classification
// comes from the pure declaration, never a second transport listing API.
func listTools(t *testing.T, r *api.Registry, caller apicontract.Caller) []apicontract.Metadata {
	t.Helper()
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Registry = r })
	f.auth.Set(caller, nil)
	w := request(f, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	var wire struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			Tools []apicontract.Metadata `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || len(wire.Error) > 0 {
		t.Fatal("catalogue request failed", w.Code, string(wire.Error))
	}
	declarations := map[string]apicontract.Metadata{}
	for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
		declarations[d.Name] = d.Metadata
	}
	for i, m := range wire.Result.Tools {
		metadata := declarations[m.Name]
		metadata.Name = m.Name
		metadata.Description = m.Description
		metadata.Annotations = m.Annotations
		metadata.InputSchema = wireSchema(m.InputSchema).(apicontract.Schema)
		metadata.OutputSchema = wireSchema(m.OutputSchema).(apicontract.Schema)
		wire.Result.Tools[i] = metadata
	}
	return wire.Result.Tools
}
func wireSchema(v any) any {
	switch v := v.(type) {
	case apicontract.Schema:
		for k, item := range v {
			v[k] = wireSchema(item)
		}
		return v
	case map[string]any:
		return wireSchema(apicontract.Schema(v))
	case []any:
		schemas := []apicontract.Schema{}
		strings := []string{}
		for i, item := range v {
			v[i] = wireSchema(item)
			if s, ok := v[i].(apicontract.Schema); ok {
				schemas = append(schemas, s)
			}
			if s, ok := item.(string); ok {
				strings = append(strings, s)
			}
		}
		if len(strings) == len(v) {
			return strings
		}
		if len(schemas) == len(v) {
			return schemas
		}
		return v
	}
	return v
}
