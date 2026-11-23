package api_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/publicerr"
)

func TestInitializeTeachesPushWaitLogsFixPromoteRelease(t *testing.T) {
	f := setup(t, nil)
	w := initialize(f)
	assertSuccess(t, w)
	var wire struct {
		Result struct {
			Instructions string `json:"instructions"`
		} `json:"result"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	instructions := wire.Result.Instructions
	if len(strings.Split(instructions, "\n")) > 16 {
		t.Fatal("agent loop is too long")
	}
	for _, phrase := range []string{
		"parked and creates no run", "-o oberth.run=1", "one background events_wait",
		"Start before pushing", "next_after_id even after timed_out", "run.finished: read run_get",
		"run_rerun once per code change", "then stop and tell the person", "none: read run_logs",
		"retry_hint.detail", "if the logs show a code fault", "show the reason, detail and log lines to the person and stop",
		"if cancelled, show who cancelled and stop without fixing code or rerunning",
		"wait (with after), retry_publication, or an unknown hint", "do not take an automatic action",
		"run_hold_list for live availability", "inspect with run_hold_exec, push the fix, then release with run_hold_abandon",
		"passed unassisted run of that exact commit", "cancel and restart the one wait",
		"display block that oberth mcp adds after each run result verbatim", "object_sha", "commit_sha",
		"publication_list", "read durable state before repeating an action", "issue.updated", "refresh issue_list or issue_get",
	} {
		if !strings.Contains(instructions, phrase) {
			t.Fatal("agent loop instruction missing", phrase)
		}
	}
}
func TestRemovedToolsHaveNoAuthorityOrCatalogue(t *testing.T) {
	f := setup(t, nil)
	for _, name := range []string{"events_since", "events_subscribe", "release_wif_reset", "release_wif_grant", "release_wif_list", "release_wif_revoke"} {
		if _, ok := apicontract.Required(name); ok {
			t.Fatal("removed action retains authority", name)
		}
		if w := rpc(f, name, map[string]any{}); !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatal("removed tool is callable", name)
		}
	}
	r, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	if len(listTools(t, r, admin())) != 74 {
		t.Fatal("removed tools changed the wrong tool count")
	}
}
func TestPromoteRequiresCommitBeforePreparing(t *testing.T) {
	f := setup(t, nil)
	for _, args := range []map[string]any{{"branch": "main"}, {"branch": "main", "sha": ""}} {
		if w := rpc(f, "promote", args); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatal("promotion without commit reached service", w.Body.String())
		}
	}
	assertSuccess(t, rpc(f, "promote", map[string]any{"branch": "main", "sha": "12345678"}))
	h := f.bindings.Promote.Handler.(*fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult])
	inputs, _, _ := h.Observed()
	if len(inputs) != 1 || inputs[0].SHA != "12345678" {
		t.Fatal("promotion commit was not required", inputs)
	}
}
func TestConstructorRequiresInjectedPrivateLogger(t *testing.T) {
	f := fake.NewClock()
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	_, err = api.New(api.Config{Auth: fake.NewAuth(admin()), Registry: r, Classify: publicerr.Classify, Runtime: f.Runtime()})
	if err == nil {
		t.Fatal("missing private logger was accepted")
	}
}

func TestHTTPOnlyRowsInvokeExactReadHandlers(t *testing.T) {
	counts := &fake.Handler[apicontract.Empty, apicontract.RunCounts]{Value: apicontract.RunCounts{Passed: 7}}
	containers := &fake.Handler[apicontract.IDArgs, apicontract.ContainerPage]{Value: apicontract.ContainerPage{Containers: []apicontract.Container{}}}
	logs := &fake.Handler[apicontract.ContainerArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: "selected native bytes"}}
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.Views = apicontract.Views{Counts: apicontract.Binding[apicontract.Empty, apicontract.RunCounts]{Handler: counts}, Containers: apicontract.Binding[apicontract.IDArgs, apicontract.ContainerPage]{Handler: containers}, ContainerLog: apicontract.Binding[apicontract.ContainerArgs, apicontract.LogResult]{Handler: logs}}
	})
	for _, tc := range []struct{ path, want string }{
		{"/api/runs/counts", `"passed":7`},
		{"/api/runs/12345678/containers", `"containers":[]`},
		{"/api/runs/12345678/containerlog?node_id=node&container=main", `"content":"selected native bytes"`},
	} {
		w := request(f, "GET", tc.path, nil)
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Fatal("HTTP view routed to a different read", tc.path, w.Body.String())
		}
	}
	countInputs, _, _ := counts.Observed()
	containerInputs, _, _ := containers.Observed()
	logInputs, _, _ := logs.Observed()
	if len(countInputs) != 1 || len(containerInputs) != 1 || len(logInputs) != 1 {
		t.Fatal("HTTP-only read preparation lost")
	}
	if containerInputs[0].ID != "12345678" || logInputs[0].ID != "12345678" || logInputs[0].NodeID != "node" || logInputs[0].Container != "main" {
		t.Fatal("HTTP-only selectors lost")
	}
}

func TestPromotionResultCarriesSourceAndResultEvidence(t *testing.T) {
	value := apicontract.PromoteResult{ID: "promotion-1", TestedRunID: "source-run", CommitSHA: "result-commit", RunID: "result-run", State: "testing"}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.Promote.Handler = &fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult]{Value: value}
		b.PromoteRetry.Handler = &fake.Handler[apicontract.PromoteRetryArgs, apicontract.PromoteResult]{Value: value}
	})
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"promote", map[string]any{"repo": "widget", "sha": "12345678"}},
		{"promote_retry", map[string]any{"id": "promotion-1"}},
	} {
		w := rpc(f, tc.name, tc.args)
		assertSuccess(t, w)
		for _, field := range []string{`"id":"promotion-1"`, `"tested_run_id":"source-run"`, `"commit_sha":"result-commit"`, `"run_id":"result-run"`, `"state":"testing"`} {
			if !strings.Contains(w.Body.String(), field) {
				t.Fatal("promotion lost evidence identity", tc.name, field, w.Body.String())
			}
		}
	}
}
