package service_test

import (
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
)

func TestDismissedAccessRequestsStayOutOfGrantLists(t *testing.T) {
	t.Parallel()
	p := people(t)
	q := secretTarget()
	approved := q
	approved.Secret = "visible/path"
	approveSecret(t, p, approved)
	call, err := p.f.s.PrepareAccessDismiss(t.Context(), adminCaller(), apicontract.AccessDismissArgs{Repo: q.Repo, Workflow: q.Workflow, Step: q.Step, Secret: q.Secret})
	if _, err = execute(t.Context(), call, err); err != nil {
		t.Fatal(err)
	}
	for _, revoked := range []bool{false, true} {
		list, err := p.f.s.PrepareAccessList(t.Context(), adminCaller(), apicontract.AccessListArgs{Repo: q.Repo, Revoked: revoked})
		got, err := execute(t.Context(), list, err)
		if err != nil || len(got.Grants) != 1 || got.Grants[0].Path != approved.Secret || got.Grants[0].Status != "approved" {
			t.Fatal("dismissed request appeared as a grant", revoked, got, err)
		}
	}
}
