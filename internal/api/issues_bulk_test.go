package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestIssueBulkMCPEncodingBoundAndFullRecords(t *testing.T) {
	// Every byte in valid encoded JSON expands by at most two bytes when
	// encoded as a JSON string; structured content is copied once unchanged.
	// Exercise escapes, HTML-sensitive text, Unicode, and multi-result overhead.
	for _, body := range []string{strings.Repeat("x", 200000), strings.Repeat("\x00\n\"\\<>&\u2028界", 5000)} {
		issue := &IssueResponse{ID: 1, Kind: "manual", State: "open", Title: "complete", Body: body}
		payload := IssueGetManyResponse{Results: []IssueGetManyResult{{ID: 1, Issue: issue}, {ID: 2, Error: "not_found"}, {ID: 3, Error: "response_limit"}}}
		encoded, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded) > IssueGetManyMaximumBytes {
			t.Fatal("fixture exceeds structured budget")
		}
		result := toolSuccess(payload)
		envelope, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		if len(envelope) > 3*len(encoded)+128 || len(envelope) > 3*IssueGetManyMaximumBytes+128 {
			t.Fatalf("unbounded tool result: %d from %d", len(envelope), len(encoded))
		}
		var textPayload IssueGetManyResponse
		content, ok := result["content"].([]map[string]string)
		if !ok || len(content) != 1 {
			t.Fatal("missing text")
		}
		if err := json.Unmarshal([]byte(content[0]["text"]), &textPayload); err != nil {
			t.Fatal(err)
		}
		if textPayload.Results[0].Issue.Body != body || len(textPayload.Results) != 3 || textPayload.Results[2].Error != "response_limit" {
			t.Fatal("text client lost complete records or fallback")
		}
	}
}

func TestIssueBulkMCPAuthenticationAndFailureClassification(t *testing.T) {
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_get_many","arguments":{"ids":[1,2]}}}`
	for _, token := range []string{"", "wrong", "valid-token"} {
		server, backend := testServer(t)
		backend.toolResult = IssueGetManyResponse{Results: []IssueGetManyResult{{ID: 1, Error: "not_found"}, {ID: 2, Issue: &IssueResponse{ID: 2, Body: "full"}}}}
		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
		if token != "" {
			request.Header.Set("Authorization", "Bearer "+token)
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if token != "valid-token" {
			if response.Code != http.StatusUnauthorized || backend.calledName != "" {
				t.Fatal("unauthenticated bulk read reached backend")
			}
			continue
		}
		if response.Code != http.StatusOK || backend.calledName != "issue_get_many" || backend.calledActor.Identity != "agent@host" || !strings.Contains(response.Body.String(), `"not_found"`) {
			t.Fatalf("authenticated bulk read: %s", response.Body.String())
		}
	}
	server, backend := testServer(t)
	backend.toolErr = errors.New("private sqlite path /private/database")
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer valid-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if !strings.Contains(response.Body.String(), `"isError":true`) || strings.Contains(response.Body.String(), "private sqlite") || !strings.Contains(response.Body.String(), "internal error (reference") {
		t.Fatalf("storage failure leaked or became partial success: %s", response.Body.String())
	}
}
