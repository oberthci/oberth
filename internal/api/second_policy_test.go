package api_test

import (
	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"strings"
	"testing"
)

func TestSecondTablePolicy(t *testing.T) {
	r, err := api.Builtin(fake.Bindings())
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range listTools(t, r, admin()) {
		if m.Mutation && (m.Annotations.ReadOnly || !m.Annotations.Destructive || m.Check == "") {
			t.Errorf("%s lacks its action declaration", m.Name)
		}
		if m.Name == "receive_outbox" && !m.Mutation {
			t.Fatal("repair advertised as read")
		}
		if m.Name == "audit_list" && m.Access != apicontract.ScopedAdmin {
			t.Fatal("repository audit hidden")
		}
	}
	f := setup(t, nil)
	c := admin()
	c.Actor.Scope = []string{"origin"}
	f.auth.Set(c, nil)
	assertSuccess(t, rpc(f, "audit_list", map[string]string{"repo": "origin/team/widget"}))
	assertSuccess(t, request(f, "GET", "/api/audit?repo=origin%2Fteam%2Fwidget", nil))
}
func TestSecondRequiredActionReasons(t *testing.T) {
	f := setup(t, nil)
	assertSuccess(t, rpc(f, "publication_abandon", map[string]string{"id": "12345678", "reason": "no longer required"}))
	if w := rpc(f, "publication_abandon", map[string]string{"id": "12345678"}); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("abandonment audit reason missing")
	}
	if w := rpc(f, "issue_close_many", map[string]any{}); !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatal("removed bulk close was exposed")
	}
}
func TestSecondNestedRequiredInputs(t *testing.T) {
	f := setup(t, nil)
	for _, tc := range []struct {
		name string
		args any
	}{
		{"workflow_capability_grant", map[string]any{"repo": "widget", "capability": map[string]any{}}},
		{"run_check", map[string]any{"repo": "widget", "files": []any{map[string]any{"path": "build.yaml"}}}},
	} {
		if w := rpc(f, tc.name, tc.args); !strings.Contains(w.Body.String(), `"isError":true`) {
			t.Fatal("incomplete nested value accepted", w.Body.String())
		}
	}
	for _, name := range []string{"branch_delete_many", "ref_delete_many"} {
		if w := rpc(f, name, map[string]any{}); !strings.Contains(w.Body.String(), `"code":-32602`) {
			t.Fatal("removed deletion tool was exposed")
		}
	}
}
