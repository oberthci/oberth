package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/validate"
)

// Traverse the advertised registry, rather than a hand-picked tool list. Every
// tool must advertise and obey an object schema. A backend regression to an
// array, nil slice, typed nil pointer or scalar must never reach structuredContent.
func TestEveryRegisteredToolOutputContract(t *testing.T) {
	for _, definition := range toolDefinitions() {
		name := definition["name"].(string)
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(definition["outputSchema"])
			if err != nil {
				t.Fatal(err)
			}
			var schema spec.Schema
			if err := json.Unmarshal(raw, &schema); err != nil {
				t.Fatal(err)
			}
			if !schema.Type.Contains("object") || len(schema.Type) != 1 {
				t.Fatalf("outputSchema must require an object: %s", raw)
			}
			// Match each collection's named array, both empty and populated. All
			// remaining responses have an extensible object schema.
			samples := []map[string]any{{}}
			if name == "issue_delete" {
				samples = []map[string]any{{"id": int64(1), "deleted": true}}
			}
			collectionSchema := schema
			if len(schema.AnyOf) > 0 {
				collectionSchema = schema.AnyOf[0]
			}
			if len(collectionSchema.Required) > 0 && name != "issue_delete" {
				key := collectionSchema.Required[0]
				samples = []map[string]any{{key: []any{}}, {key: []any{map[string]any{"id": "example"}}}}
				if len(schema.AnyOf) > 0 {
					samples = append(samples, map[string]any{"declarations": []any{}})
				}
			}
			for _, payload := range samples {
				result := callOutputContract(t, name, payload)
				if result.IsError {
					t.Fatalf("object result refused: %s", result.Content)
				}
				var object map[string]any
				if err := json.Unmarshal(result.Structured, &object); err != nil || object == nil {
					t.Fatalf("expected object: %s (%v)", result.Structured, err)
				}
				if err := validate.AgainstSchema(&schema, object, strfmt.Default); err != nil {
					t.Fatalf("schema mismatch: %v; result=%s", err, result.Structured)
				}
			}
			var nilMap map[string]any
			var nilPointer *IssueResponse
			for _, invalid := range []any{json.RawMessage("null"), []any{}, []any{map[string]any{}}, []string(nil), nilMap, nilPointer, "secret-value", 42} {
				result := callOutputContract(t, name, invalid)
				if !result.IsError || len(result.Structured) != 0 {
					t.Fatalf("invalid %T escaped object boundary: %+v", invalid, result)
				}
				if strings.Contains(string(result.Content), "secret-value") {
					t.Fatal("invalid payload leaked")
				}
			}
		})
	}
}

type outputContractResult struct {
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
	Content    json.RawMessage `json:"content"`
}

func callOutputContract(t *testing.T, name string, payload any) outputContractResult {
	t.Helper()
	server, backend := testServer(t)
	backend.toolResult = payload
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":{}}}`, name)))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result outputContractResult `json:"result"`
		Error  *rpcError            `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || envelope.Error != nil || backend.calledName != name {
		t.Fatalf("tool was not exercised: %s", response.Body.String())
	}
	return envelope.Result
}
