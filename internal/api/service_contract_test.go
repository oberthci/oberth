package api_test

import (
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/logcontract"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stepmodel"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestLogRuntimeUsesPureReadValues(t *testing.T) {
	if reflect.TypeFor[runlog.ReadRequest]() != reflect.TypeFor[logcontract.ReadRequest]() || reflect.TypeFor[runlog.Meta]() != reflect.TypeFor[logcontract.Meta]() {
		t.Fatal("log runtime owns a second read value")
	}
}
func TestRepositoryResultCarriesCountAndLatestRun(t *testing.T) {
	count := 2
	latest := &stepmodel.RunSummary{ID: "12345678"}
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RepoList.Handler = &fake.Handler[apicontract.RepoListArgs, apicontract.RepoPage]{Value: apicontract.RepoPage{Repositories: []apicontract.Repository{{Repo: "origin/team/widget", BranchCount: &count, LatestRun: latest}}}}
	})
	for _, w := range []*httptest.ResponseRecorder{rpc(f, "repo_list", map[string]any{}), request(f, "GET", "/api/repos", nil)} {
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), `"branch_count":2`) || !strings.Contains(w.Body.String(), `"latest_run":{"id":"12345678"`) {
			t.Fatal("repository facts were lost", w.Body.String())
		}
	}
}
func TestPurgedLogOperationReasonAndDetailRemainWhole(t *testing.T) {
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
		b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{DisplayReason: "logs purged", Detail: "Retention removed these bytes."}}
	})
	for _, w := range []*httptest.ResponseRecorder{rpc(f, "run_logs", map[string]string{"id": "12345678"}), request(f, "GET", "/api/runs/12345678/logs", nil)} {
		assertSuccess(t, w)
		if !strings.Contains(w.Body.String(), `"display_reason":"logs purged"`) || !strings.Contains(w.Body.String(), `"detail":"Retention removed these bytes."`) {
			t.Fatal("purged log state was lost", w.Body.String())
		}
	}
}
func TestServiceAuthorityMatchesBoundRuntimeCatalogue(t *testing.T) {
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	read := admin()
	read.Actor.Admin = false
	scoped := admin()
	scoped.Actor.Scope = []string{"origin"}
	metrics := read
	metrics.Actor.Scope = []string{"metrics"}
	for _, caller := range []apicontract.Caller{admin(), read, scoped, metrics} {
		visible := map[string]bool{}
		for _, m := range listTools(t, r, caller) {
			visible[m.Name] = true
		}
		for _, d := range apicontract.Definitions(apicontract.Bindings{}) {
			need, ok := apicontract.Required(d.Name)
			if !ok || visible[d.Name] != need.Allows(caller.Actor) {
				t.Fatal("runtime visibility differs from service authority", d.Name, caller.Actor.Scope)
			}
		}
	}
}

func TestActionReceiptKeepsCallerWarningAndSchema(t *testing.T) {
	for _, warning := range []string{"Repeat cancel to skip exit handlers.", "Publication may be partial.", "Release steps may run again."} {
		f := setup(t, func(_ *api.Config, b *apicontract.Bindings) {
			b.RunCancel.Handler = &fake.Handler[apicontract.RunCancelArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "12345678", Warning: warning}}
			b.RunRerun.Handler = &fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt]{Value: apicontract.Receipt{ID: "12345678", Warning: warning}}
		})
		for _, name := range []string{"run_cancel", "run_rerun"} {
			w := rpc(f, name, map[string]string{"id": "12345678"})
			assertSuccess(t, w)
			if !strings.Contains(w.Body.String(), `"warning":"`+warning+`"`) {
				t.Fatal("action warning was lost", name, w.Body.String())
			}
		}
		r, err := api.Builtin(f.bindings)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range listTools(t, r, admin()) {
			if m.Name == "run_cancel" || m.Name == "run_rerun" {
				if p, ok := m.OutputSchema["properties"].(apicontract.Schema)["warning"].(apicontract.Schema); !ok || p["type"] != "string" {
					t.Fatal("warning missing from declared result schema", m.Name)
				}
			}
		}
	}
}
