package api_test

import (
	"encoding/json"
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stepmodel"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUIBrowserFlowOverHTTP(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunList.Handler = &fake.Handler[apicontract.RunListArgs, apicontract.RunPage]{Value: apicontract.RunPage{Runs: []apicontract.RunRecord{{RunSummary: stepmodel.RunSummary{ID: "12345678", DisplayReason: strings.Repeat("r", 40)}}}}}
		b.IssueGet.Handler = &fake.Handler[apicontract.IssueGetArgs, model.Issue]{Value: model.Issue{ID: 42, Title: "<script>untrusted</script>", Body: "<b>plain text</b>"}}
	})
	for _, path := range []string{
		"/api/repos", "/api/runs?limit=50", "/api/runs?limit=50&repo=origin/team/widget&ref=main&status=passed&before=12345678",
		"/api/runs/12345678?view=dashboard&step_limit=100&step_offset=0",
		"/api/runs/12345678/logs?step=build&tail=true&limit=500",
		"/api/runs/12345678/logs?step=build&tail=true&limit=500&byte_offset=0",
		"/api/runs/12345678/containers", "/api/runs/12345678/containerlog?node_id=node&container=main&tail=true&limit=500",
		"/api/runs/12345678/containerlog/stream?node_id=node&container=main",
		"/api/repos?archived=include", "/api/issues?state=open&limit=50", "/api/issues?state=open&limit=50&before=42", "/api/issues/42", "/api/status", "/api/runs/counts",
	} {
		assertSuccess(t, request(f, "GET", path, nil))
	}
	assertSuccess(t, rpc(f, "events_wait", map[string]any{"timeout_s": 25, "after_id": 0, "run_ids": []string{"12345678"}}))
	assertSuccess(t, rpc(f, "run_hold_list", map[string]any{"id": "12345678"}))
}

func TestUIRefusalAndAuthenticationFlow(t *testing.T) {
	refused := publicerr.New(publicerr.Conflict, "This run cannot be cancelled.", "Read the current run.", nil)
	refused.Detail = "complete server detail"
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunCancel.Handler = &fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt]{Error: refused}
	})
	w := rpc(f, "run_cancel", map[string]any{"id": "12345678", "reason": "web UI"})
	var reply struct {
		Result struct {
			IsError bool
			Content []struct{ Text string }
		}
	}
	if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
		t.Fatal(err)
	}
	var got struct{ Reason, Remedy, Detail string }
	if err := json.Unmarshal([]byte(reply.Result.Content[0].Text), &got); err != nil {
		t.Fatal(err)
	}
	if !reply.Result.IsError || got.Reason != refused.Reason() || got.Remedy != refused.Remedy() || got.Detail != refused.Detail {
		t.Fatal("refusal changed", w.Body.String())
	}
	bad := httptest.NewRequest("GET", "/api/issues/42", nil)
	bad.Header.Set("Authorization", "Bearer invalid")
	unauthorized := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(unauthorized, bad)
	if unauthorized.Code != 401 {
		t.Fatal("bad token accepted", unauthorized.Code)
	}
	f.auth.Set(apicontract.Caller{}, publicerr.New(publicerr.Forbidden, "Invalid credentials.", "Use a current credential.", nil))
	if w = request(f, "GET", "/api/issues/42", nil); w.Code != 401 || !strings.Contains(w.Body.String(), "Caller credentials are invalid.") {
		t.Fatal("auth refusal missing", w.Code, w.Body.String())
	}
}
