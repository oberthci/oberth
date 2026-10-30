package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/validate"
)

func TestCompactReadInputSchemas(t *testing.T) {
	for _, name := range []string{"issue_get", "issue_get_many", "run_get"} {
		t.Run(name, func(t *testing.T) {
			var schema spec.Schema
			for _, definition := range toolDefinitions() {
				if definition["name"] == name {
					raw, _ := json.Marshal(definition["inputSchema"])
					if err := json.Unmarshal(raw, &schema); err != nil {
						t.Fatal(err)
					}
				}
			}
			var good, bad []string
			switch name {
			case "issue_get":
				good = []string{`{"id":1}`, `{"id":1,"fields":["id","state","occurrences"],"body_limit":0}`}
				bad = []string{`{"id":1,"fields":[]}`, `{"id":1,"fields":["body","body"]}`, `{"id":1,"fields":["CIWorkSequence"]}`, `{"id":1,"body_limit":-1}`, `{"id":1,"body_limit":1.5}`}
			case "issue_get_many":
				good = []string{`{"ids":[1,2]}`, `{"ids":[1,2],"fields":["id","title"],"body_limit":4096}`}
				bad = []string{`{"ids":[1],"fields":["Body"]}`, `{"ids":[1],"fields":[]}`, `{"ids":[1],"body_limit":-1}`}
			case "run_get":
				good = []string{`{"id":"12345678"}`, `{"id":"12345678","summary":true}`, `{"id":"12345678","summary":false}`}
				bad = []string{`{"id":"12345678","summary":"true"}`, `{"id":"12345678","summary":1}`}
			}
			for _, raw := range append(good, bad...) {
				var args map[string]any
				if err := json.Unmarshal([]byte(raw), &args); err != nil {
					t.Fatal(err)
				}
				err := validate.AgainstSchema(&schema, args, strfmt.Default)
				wantValid := false
				for _, valid := range good {
					wantValid = wantValid || raw == valid
				}
				if (err == nil) != wantValid {
					t.Errorf("schema %s: %v (want valid=%t)", raw, err, wantValid)
				}
			}
		})
	}
}

func TestCompactMCPAuthenticationArgumentsAndBothOutputs(t *testing.T) {
	for _, test := range []struct {
		name, args string
		payload    any
	}{
		{"issue_append", `{"id":1,"text":"checkpoint"}`, IssueMutationResponse{ID: 1, State: "open", UpdatedAt: "2026-10-30T00:00:00Z", Occurrences: 1, AppendedBytes: 10}},
		{"issue_close", `{"id":1}`, IssueMutationResponse{ID: 1, State: "closed"}},
		{"issue_update", `{"id":1,"title":"new","body":"updated"}`, IssueMutationResponse{ID: 1, State: "open"}},
		{"issue_get", `{"id":1,"fields":["id","state"],"body_limit":0}`, map[string]any{"id": 1, "state": "open"}},
		{"issue_get_many", `{"ids":[1],"fields":["id","body"],"body_limit":0}`, IssueGetManyResponse{Results: []IssueGetManyResult{{ID: 1, Issue: map[string]any{"id": 1, "body": "\n[truncated 500000 bytes]"}}}}},
		{"run_get", `{"id":"12345678","summary":true}`, map[string]any{"id": "1234567890abcdef", "status": "failed", "phase": "test", "failed_burn": "test", "failed_step": "unit", "error": "exit 1", "steps": []any{}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, authenticated := range []bool{false, true} {
				server, backend := testServer(t)
				backend.toolResult = test.payload
				body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":%q,"arguments":%s}}`, test.name, test.args)
				request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
				if authenticated {
					request.Header.Set("Authorization", "Bearer valid-token")
				}
				response := httptest.NewRecorder()
				server.Handler().ServeHTTP(response, request)
				if !authenticated {
					if response.Code != http.StatusUnauthorized || backend.calledName != "" {
						t.Fatal("unauthenticated compact call reached backend")
					}
					continue
				}
				if response.Code != http.StatusOK || backend.calledName != test.name || backend.calledActor.Identity != "agent@host" {
					t.Fatalf("call failed: %s", response.Body.String())
				}
				var envelope struct {
					Result struct {
						Content []struct {
							Text string `json:"text"`
						} `json:"content"`
						Structured json.RawMessage `json:"structuredContent"`
						IsError    bool            `json:"isError"`
					} `json:"result"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
					t.Fatal(err)
				}
				expected, _ := json.Marshal(test.payload)
				if envelope.Result.IsError || len(envelope.Result.Content) != 1 || envelope.Result.Content[0].Text != string(expected) || string(envelope.Result.Structured) != string(expected) {
					t.Fatalf("compact output differs across clients: %s", response.Body.String())
				}
				var wantArgs, gotArgs any
				if err := json.Unmarshal([]byte(test.args), &wantArgs); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(backend.calledArguments, &gotArgs); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(wantArgs, gotArgs) {
					t.Fatalf("arguments changed: %s", backend.calledArguments)
				}
			}
		})
	}
}
