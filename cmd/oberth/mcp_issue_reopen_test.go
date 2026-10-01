package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
)

func TestMCPIssueReopenRejectsCIWithActionableErrorWithoutMutation(t *testing.T) {
	ctx := t.Context()
	database, err := store.Open(ctx, filepath.Join(t.TempDir(), "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{Name: "origin", Kind: "forgejo", BaseURL: "https://forge.example.test/org"})
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.CreateRepository(ctx, model.RepositorySpec{Name: "repo", UpstreamID: upstream.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	issue, err := database.UpsertCIIssue(ctx, "tooling-test@host", repo.ID, "feature", "red", "failure")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.CloseCIIssue(ctx, "tooling-test@host", issue.ID, "resolved"); err != nil {
		t.Fatal(err)
	}
	beforeIssue, err := database.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeAudit, err := database.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}

	control, err := service.NewAPI(service.APIConfig{Runs: database, Issues: database})
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.New(toolingAuthenticator{}, control, control, "test", api.WithErrorClassifier(classifyViewError))
	if err != nil {
		t.Fatal(err)
	}
	requestBody := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"issue_reopen","arguments":{"id":%d}}}`, issue.ID)
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(requestBody))
	request.Header.Set("Authorization", "Bearer fixture")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(envelope.Error) != 0 || !envelope.Result.IsError || len(envelope.Result.Content) != 1 {
		t.Fatalf("MCP response: %d %s", response.Code, response.Body)
	}
	want := "service: invalid input: only manual issues can be reopened; CI state follows runs"
	if got := envelope.Result.Content[0].Text; got != want {
		t.Fatalf("MCP error = %q, want %q", got, want)
	}
	afterIssue, err := database.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterAudit, err := database.VerifyAuditChain(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(afterIssue, beforeIssue) || !reflect.DeepEqual(afterAudit, beforeAudit) {
		t.Fatalf("CI refusal mutated issue or audit: issue %#v -> %#v, audit %#v -> %#v", beforeIssue, afterIssue, beforeAudit, afterAudit)
	}
}
