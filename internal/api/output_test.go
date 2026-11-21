package api_test

import (
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestResponseByteLimitsAcceptNRefuseNPlusOne(t *testing.T) {
	base := stepmodel.RunSummary{ID: "12345678"}
	encoded, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	for _, extra := range []int{0, 1} {
		value := base
		value.Detail = strings.Repeat("a", (4<<20)-len(encoded)-len(`,"detail":""`)+extra)
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: value}
		})
		registry, err := api.Builtin(f.bindings)
		if err != nil {
			t.Fatal(err)
		}
		_ = registry
		w := rpc(f, "run_get", map[string]string{"id": "12345678"})
		if strings.Contains(w.Body.String(), `"isError":true`) {
			err = fmt.Errorf("refused")
		}
		if (err != nil) != (extra == 1) {
			t.Fatalf("response N+%d: %v", extra, err)
		}
	}
}
func TestWireCatalogueCannotBeMutatedByCaller(t *testing.T) {
	b := fake.Bindings()
	r, err := api.Builtin(b)
	if err != nil {
		t.Fatal(err)
	}
	tools := listTools(t, r, admin())
	before := tools[0].InputSchema["type"]
	tools[0].InputSchema["type"] = "broken"
	if listTools(t, r, admin())[0].InputSchema["type"] != before {
		t.Fatal("listing changed registry")
	}
}

func TestUIHandoffPreservesToolRunIdentityAndPlan(t *testing.T) {
	run := stepmodel.RunSummary{ID: "12345678", Repo: "origin/team/widget", ObjectSHA: "tag-object", CommitSHA: new("tested-commit"), Status: stepmodel.RunRunning, Steps: []stepmodel.RunStep{{Name: "authored-build", Path: "entry/build", Status: stepmodel.StepPending}}}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: run}
	})
	w := request(f, "GET", "/api/runs/12345678?view=dashboard", nil)
	assertSuccess(t, w)
	for _, fact := range []string{"12345678", "authored-build", "entry/build", "tag-object", "tested-commit"} {
		if !strings.Contains(w.Body.String(), fact) {
			t.Fatal("UI read lost a shared fact")
		}
	}
	if strings.Contains(w.Body.String(), `"sha":`) {
		t.Fatal("invented identity alias")
	}
}
func TestEveryOutputCollectionIsAnArray(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct{ tool, key string }{{"run_list", "runs"}, {"repo_list", "repositories"}, {"access_list", "grants"}, {"issue_list", "issues"}, {"promotion_list", "promotions"}} {
		w := rpc(f, tc.tool, map[string]any{})
		assertSuccess(t, w)
		var response struct {
			Result struct {
				Content json.RawMessage `json:"structuredContent"`
			} `json:"result"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(response.Result.Content), `"`+tc.key+`":[]`) {
			t.Fatal(w.Body.String())
		}
	}
}

func TestLogObservationStatesStayDistinctAndWhole(t *testing.T) {
	for _, reason := range []string{"step waiting to start", "step is running", "container capture incomplete", "run log byte limit reached", "log was purged", "step is unknown", "retained log bytes are unavailable"} {
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Meta: runlog.Meta{Reason: reason, Detail: "full observation detail"}}}
		})
		w := rpc(f, "run_logs", map[string]any{"id": "12345678"})
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), reason) || !strings.Contains(w.Body.String(), "full observation detail") {
			t.Fatal("log state was flattened or lost")
		}
	}
}
func TestRunDisplayStatesRemainSharedValues(t *testing.T) {
	for _, state := range []stepmodel.RunStatus{stepmodel.RunQueued, stepmodel.RunRunning, stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: stepmodel.RunSummary{ID: "12345678", Status: state}}
		})
		w := request(f, "GET", "/api/runs/12345678", nil)
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), `"status":"`+string(state)+`"`) {
			t.Fatal("run display state changed")
		}
	}
}
func TestRegistryInventoriesMatchTheApprovedOperationTable(t *testing.T) {
	text, err := os.ReadFile("../../docs/plans/c1-transport-and-tool-registry.md")
	if err != nil {
		t.Fatal(err)
	}
	_, table, ok := strings.Cut(string(text), "The tool registry exposes these operation families:")
	if !ok {
		t.Fatal("operation inventory is absent")
	}
	table, _, ok = strings.Cut(table, "This is an operation inventory")
	if !ok {
		t.Fatal("operation inventory end is absent")
	}
	want := map[string]bool{}
	for _, match := range regexp.MustCompile("`([a-z][a-z0-9_]*)`").FindAllStringSubmatch(table, -1) {
		want[match[1]] = true
	}
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"branch_delete_many", "branch_deletion_list", "branch_deletion_resolve", "ref_delete_many", "issue_close_many", "issue_unlock", "secretstore_plan", "secretstore_sync_receipt", "secretstore_verify", "promote_grant", "promote_grant_list", "promote_grant_revoke", "events_since", "events_subscribe"} {
		delete(want, name)
	}
	want["publication_list"] = true
	for _, m := range listTools(t, r, admin()) {
		if !want[m.Name] {
			t.Fatalf("undeclared operation %s", m.Name)
		}
		delete(want, m.Name)
	}
	if len(want) != 0 {
		t.Fatalf("missing operations: %v", want)
	}
}

func TestMCPFramingPreservesAFullEscapedPayload(t *testing.T) {
	base := stepmodel.RunSummary{ID: "12345678"}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Detail = strings.Repeat(`"`, ((4<<20)-len(raw)-len(`,"detail":""`))/2)
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, stepmodel.RunSummary]{Value: base}
	})
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": strings.Repeat("a", 128<<10), "method": "tools/call", "params": map[string]any{"name": "run_get", "arguments": map[string]string{"id": "12345678"}}})
	if err != nil {
		t.Fatal(err)
	}
	w := request(f, "POST", "/mcp", body)
	assertSuccess(t, w)
	if !strings.Contains(w.Body.String(), `"structuredContent":`) {
		t.Fatal("framing lost the full typed result")
	}
}
