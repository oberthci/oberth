package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-openapi/spec"
	"github.com/go-openapi/strfmt"
	"github.com/go-openapi/validate"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// Exercise real service results through MCP, including nil store lists and
// successful deletion. Validate against the schema clients actually receive.
func TestMCPRealResultSchemas(t *testing.T) {
	f := newControlFixture(t)
	grants := &stubPromotionGrantStore{}
	control := f.apiWithGrants(t, grants)
	server := resultServer(t, control, api.Actor{Identity: "agent@host"})
	schemas := resultSchemas(t, server)
	for _, populated := range []bool{false, true} {
		if populated {
			grants.grants = []model.PromotionGrant{{ID: "grant-1", RepoID: f.repo.ID}}
		}
		result := resultCall(t, server, "promote_grant_list", map[string]any{})
		validateResult(t, schemas["promote_grant_list"], result)
		var response api.PromotionGrantListResponse
		if err := json.Unmarshal(result.Structured, &response); err != nil {
			t.Fatal(err)
		}
		if response.Grants == nil || len(response.Grants) != len(grants.grants) {
			t.Fatalf("grants were not retained as an array: %s", result.Structured)
		}
		if result.Content[0].Text != string(result.Structured) {
			t.Fatal("text and structured content differ")
		}
	}
	issue, err := f.store.CreateManualIssue(t.Context(), "agent@host", model.ManualIssueSpec{Title: "delete me", Body: "body"})
	if err != nil {
		t.Fatal(err)
	}
	result := resultCall(t, server, "issue_delete", map[string]any{"id": issue.ID})
	validateResult(t, schemas["issue_delete"], result)
	var deleted api.IssueDeleteResponse
	if err := json.Unmarshal(result.Structured, &deleted); err != nil {
		t.Fatal(err)
	}
	if !deleted.Deleted || deleted.ID != issue.ID {
		t.Fatalf("unexpected deletion: %s", result.Structured)
	}
	if _, err := f.store.Issue(t.Context(), issue.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("issue still exists: %v", err)
	}
}

func TestMCPRefusalReasonsAndScope(t *testing.T) {
	f := newControlFixture(t)
	actor := api.Actor{Identity: "agent@host"}
	args := map[string]any{"sha": strings.Repeat("a", 40), "branch": "main"}
	t.Run("admin required", func(t *testing.T) {
		result := resultCall(t, resultServer(t, f.api(t), actor), "promote", args)
		assertRefusal(t, result, "forbidden: promote requires an admin uplink; mint one with: oberth uplink add --admin", "")
	})
	t.Run("missing grant", func(t *testing.T) {
		seedGreenPromotionCandidate(t, f, strings.Repeat("a", 40))
		result := resultCall(t, resultServer(t, f.apiWithGrants(t, &stubPromotionGrantStore{}), actor), "promote", args)
		assertRefusal(t, result, "forbidden: no matching promotion grant; request one from an admin with promote_grant", "")
	})
	t.Run("unavailable capability", func(t *testing.T) {
		result := resultCall(t, resultServer(t, f.api(t), actor), "promote_grant_list", map[string]any{})
		assertRefusal(t, result, "service unavailable: promotion grants", "")
	})
	t.Run("transient audit contention", func(t *testing.T) {
		control := f.api(t)
		gateErr := fmt.Errorf("%w: /private/database.sqlite", store.ErrAuditVerificationBusy)
		control.mutationGate = func(context.Context) error { return gateErr }
		_, err := control.CallTool(t.Context(), actor, "promote", json.RawMessage(`{}`))
		if !errors.Is(err, ErrUnavailable) || !errors.Is(err, store.ErrAuditVerificationBusy) {
			t.Fatalf("contention lost its transient/unavailable classification: %v", err)
		}
		result := resultCall(t, resultServer(t, control, actor), "promote", args)
		assertRefusal(t, result, "service unavailable: transient audit verification contention; retry the request", "/private/database.sqlite")
	})
	t.Run("audit integrity mismatch", func(t *testing.T) {
		control := f.api(t)
		control.mutationGate = func(context.Context) error { return store.ErrAuditStateChanged }
		result := resultCall(t, resultServer(t, control, actor), "promote", args)
		assertRefusal(t, result, "service unavailable", "transient")
	})
	t.Run("out of scope equals missing", func(t *testing.T) {
		scope := newScopeFixture(t)
		scope.service.sizeGrants = scope.store
		scope.service.git = f.git
		scope.service.promotionRuns = scope.store
		scope.service.enqueues = f.scheduler
		for _, actor := range []api.Actor{scopedActor, scopedAdmin} {
			server := resultServer(t, scope.service, actor)
			hiddenPromotion := resultCall(t, server, "promote", map[string]any{"repo": hiddenRepoName, "sha": scope.runs["codeberg"][0].SHA, "branch": "main"})
			missingPromotion := resultCall(t, server, "promote", map[string]any{"repo": "does-not-exist", "sha": strings.Repeat("e", 40), "branch": "main"})
			assertRefusal(t, hiddenPromotion, "not found", hiddenRepoName)
			if fmt.Sprint(hiddenPromotion.Content) != fmt.Sprint(missingPromotion.Content) {
				t.Fatal("promotion revealed an out-of-scope repository")
			}

			for _, tool := range []string{"promote_grant_list", "size_grant_get", "kubevirt_grant_get"} {
				hidden := resultCall(t, server, tool, map[string]any{"repo": hiddenRepoName})
				missing := resultCall(t, server, tool, map[string]any{"repo": "does-not-exist"})
				if !hidden.IsError || fmt.Sprint(hidden.Content) != fmt.Sprint(missing.Content) {
					t.Fatalf("%s distinguishes hidden from missing: %+v / %+v", tool, hidden, missing)
				}
				if strings.Contains(fmt.Sprint(hidden.Content), hiddenRepoName) {
					t.Fatalf("%s leaked hidden repo", tool)
				}
			}
		}
	})
	t.Run("raw dependency remains opaque", func(t *testing.T) {
		control := f.apiWithGrants(t, &failingGrantLookup{err: errors.New("token=secret-value hidden-repo /private/database.sqlite")})
		result := resultCall(t, resultServer(t, control, actor), "promote", args)
		assertRefusal(t, result, "internal error (reference ", "secret-value")
	})
}

type failingGrantLookup struct {
	stubPromotionGrantStore
	err error
}

func (f *failingGrantLookup) MatchingPromotionGrant(context.Context, int64, string, string) (model.PromotionGrant, error) {
	return model.PromotionGrant{}, f.err
}

type mcpResult struct {
	Structured json.RawMessage `json:"structuredContent"`
	IsError    bool            `json:"isError"`
	Content    []struct {
		Text string `json:"text"`
	} `json:"content"`
}

func resultServer(t *testing.T, control *API, actor api.Actor) *api.Server {
	t.Helper()
	server, err := api.New(auditSurfaceAuth{actor}, control, control, "test", api.WithErrorClassifier(func(err error) (int, string) {
		switch {
		case errors.Is(err, store.ErrNotFound):
			return http.StatusNotFound, "not found"
		case errors.Is(err, ErrForbidden):
			return http.StatusForbidden, "forbidden"
		case errors.Is(err, ErrUnavailable):
			return http.StatusServiceUnavailable, "service unavailable"
		default:
			return http.StatusInternalServerError, "internal error"
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	return server
}
func resultRPC(t *testing.T, server *api.Server, method string, params any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(raw)))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var envelope struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || len(envelope.Error) > 0 {
		t.Fatalf("RPC failed: %s", response.Body.String())
	}
	return envelope.Result
}
func resultCall(t *testing.T, server *api.Server, tool string, args any) mcpResult {
	t.Helper()
	raw := resultRPC(t, server, "tools/call", map[string]any{"name": tool, "arguments": args})
	var result mcpResult
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func resultSchemas(t *testing.T, server *api.Server) map[string]spec.Schema {
	t.Helper()
	var response struct {
		Tools []struct {
			Name   string      `json:"name"`
			Schema spec.Schema `json:"outputSchema"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(resultRPC(t, server, "tools/list", nil), &response); err != nil {
		t.Fatal(err)
	}
	schemas := map[string]spec.Schema{}
	for _, tool := range response.Tools {
		schemas[tool.Name] = tool.Schema
	}
	return schemas
}
func validateResult(t *testing.T, schema spec.Schema, result mcpResult) {
	t.Helper()
	if result.IsError {
		t.Fatalf("tool error: %+v", result.Content)
	}
	var object map[string]any
	if err := json.Unmarshal(result.Structured, &object); err != nil || object == nil {
		t.Fatalf("expected object: %s (%v)", result.Structured, err)
	}
	if err := validate.AgainstSchema(&schema, object, strfmt.Default); err != nil {
		t.Fatalf("schema mismatch: %v", err)
	}
}
func assertRefusal(t *testing.T, result mcpResult, want, absent string) {
	t.Helper()
	if !result.IsError || len(result.Structured) > 0 || len(result.Content) != 1 || !strings.HasPrefix(result.Content[0].Text, want) {
		t.Fatalf("unexpected refusal: %+v", result)
	}
	if absent != "" && strings.Contains(result.Content[0].Text, absent) {
		t.Fatalf("refusal leaked %q", absent)
	}
}

// Real service projections must keep the named arrays promised by every list
// schema, independently of the transport's final object guard.
func TestMCPRealCollectionSchemas(t *testing.T) {
	f := newScopeFixture(t)
	f.service.uplinkLifecycle = &lifecycleStub{}
	f.service.issueLifecycle = f.store
	f.service.schedules = &recordingScheduleControl{store: f.store}
	server := resultServer(t, f.service, unscopedActor)
	schemas := resultSchemas(t, server)
	cases := map[string]map[string]any{
		"run_list": {}, "repo_list": {}, "promotion_list": {}, "issue_list": {},
		"issue_get_many":   {"ids": []int64{f.global.ID}},
		"issue_close_many": {"ids": []int64{f.global.ID}, "reason": "schema test"},
		"artifacts":        {"id": f.runs["github"][0].ID}, "schedule_list": {},
		"uplink_list": {}, "audit_list": {}, "access_list": {},
		"kubevirt_grant_list": {}, "promote_grant_list": {},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) { validateResult(t, schemas[name], resultCall(t, server, name, args)) })
	}
	validateResult(t, schemas["access_list"], resultCall(t, server, "access_list", map[string]any{"pending": true}))
}
