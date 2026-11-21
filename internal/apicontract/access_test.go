package apicontract_test

import (
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"testing"
)

func TestAuthorityClassesAndUnknownActions(t *testing.T) {
	for _, tc := range []struct {
		scope                         []string
		admin                         bool
		read, scoped, global, metrics bool
	}{
		{nil, false, true, false, false, false},
		{[]string{"repository"}, true, true, true, false, false},
		{nil, true, true, true, true, true},
		{[]string{"metrics"}, false, false, false, false, true},
		{[]string{"metrics"}, true, false, false, false, true},
	} {
		a := actor.Actor{Admin: tc.admin, Scope: tc.scope}
		for _, check := range []struct {
			class apicontract.Access
			want  bool
		}{{apicontract.Read, tc.read}, {apicontract.ScopedAdmin, tc.scoped}, {apicontract.Admin, tc.global}, {apicontract.Metrics, tc.metrics}} {
			if !check.class.Valid() || check.class.Allows(a) != check.want {
				t.Fatal("authority class differs", check.class, tc)
			}
		}
	}
	if apicontract.Access("unknown").Valid() || apicontract.Access("unknown").Allows(actor.Actor{Admin: true}) {
		t.Fatal("unknown class grants authority")
	}
	for _, name := range []string{"unknown", "repos", "issue_close_many", "secretstore_verify"} {
		if _, ok := apicontract.Required(name); ok {
			t.Fatal("unknown or removed action accepted", name)
		}
	}
}
func TestRequiredUsesEveryDeclaration(t *testing.T) {
	definitions := apicontract.Definitions(apicontract.Bindings{})
	if len(definitions) != 74 {
		t.Fatal("tool inventory changed", len(definitions))
	}
	for _, d := range definitions {
		got, ok := apicontract.Required(d.Name)
		if !ok || got != d.Access {
			t.Fatal("action class differs from tool declaration", d.Name, got, d.Access)
		}
	}
	for _, tc := range []struct {
		name   string
		access apicontract.Access
	}{{"push", apicontract.Read}, {"backup", apicontract.Admin}, {"metrics", apicontract.Metrics}, {"run_get", apicontract.Read}, {"audit_list", apicontract.ScopedAdmin}, {"repo_add", apicontract.Admin}} {
		got, ok := apicontract.Required(tc.name)
		if !ok || got != tc.access {
			t.Fatal("action authority differs", tc.name, got)
		}
	}
}
