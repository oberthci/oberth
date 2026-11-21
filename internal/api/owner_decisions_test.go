package api_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestOwnerDropsPromotionGrantTools(t *testing.T) {
	f := setup(t, nil)
	r, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"promote_grant", "promote_grant_list", "promote_grant_revoke", "branch_delete_many", "ref_delete_many"} {
		if _, ok := apicontract.Required(name); ok {
			t.Fatal("removed action retains authority", name)
		}
		for _, m := range listTools(t, r, admin()) {
			if m.Name == name {
				t.Fatal("removed tool is advertised", name)
			}
		}
		if w := rpc(f, name, map[string]any{}); !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatal("removed tool is callable", name, w.Body.String())
		}
	}
}

func TestOwnerDropsOptionalActionFields(t *testing.T) {
	f := setup(t, nil)
	r, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	assertSuccess(t, rpc(f, "promote", map[string]any{"branch": "main", "sha": "12345678"}))
	assertSuccess(t, rpc(f, "run_rerun", map[string]any{"id": "12345678"}))
	for _, tc := range []struct {
		tool, key string
		value     any
	}{
		{"promote", "prune_source", true},
		{"promote", "sources", []any{map[string]any{"sha": "12345678"}}},
		{"promote", "skip_conflicting", true},
		{"promote", "tag_after", map[string]any{"name": "v1", "message": "Publish the tested source."}},
		{"promote", "delete_branches", []string{"work"}},
		{"promote", "delete_refs", []string{"refs/heads/work"}},
		{"run_rerun", "force", true},
	} {
		args := map[string]any{"branch": "main", "sha": "12345678"}
		if tc.tool == "run_rerun" {
			args = map[string]any{"id": "12345678"}
		}
		assertDroppedFieldSchema(t, r, tc.tool, tc.key)
		args[tc.key] = tc.value
		if w := rpc(f, tc.tool, args); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatal("removed field is accepted", tc.tool, tc.key, w.Body.String())
		}
	}
	// Cancel and schedule force are separate, retained operations.
	assertSuccess(t, rpc(f, "run_cancel", map[string]any{"id": "12345678", "force": true}))
	assertSuccess(t, rpc(f, "schedule_trigger", map[string]any{"repo": "widget", "entry": "nightly", "force": true}))
	p := f.bindings.Promote.Handler.(*fake.Handler[apicontract.PromoteArgs, apicontract.PromoteResult])
	rerun := f.bindings.RunRerun.Handler.(*fake.Handler[apicontract.RunRerunArgs, apicontract.Receipt])
	inputs, _, _ := p.Observed()
	reruns, _, _ := rerun.Observed()
	if len(inputs) != 1 || len(reruns) != 1 {
		t.Fatal("removed fields reached service preparation")
	}
}

func assertDroppedFieldSchema(t *testing.T, r *api.Registry, tool, key string) {
	t.Helper()
	for _, m := range listTools(t, r, admin()) {
		if m.Name == tool {
			if _, ok := m.InputSchema["properties"].(apicontract.Schema)[key]; ok {
				t.Fatal("removed field is advertised", tool, key)
			}
		}
	}
}
