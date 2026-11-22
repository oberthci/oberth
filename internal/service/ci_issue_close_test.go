package service_test

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// The installation administrator clears a CI issue that no later run will
// close; an uplink, a scoped administrator included, still cannot, and a later failing default-branch run reopens it as usual.
func TestAdministratorClosesCIIssue(t *testing.T) {
	t.Parallel()
	p := people(t)
	f := p.f
	f.c.Runs, f.c.Catalog, f.c.Events = p.state, p.state, p.state
	f.restart(t)
	runs := acceptCISequence(t, p)
	if e := f.s.Finished(t.Context(), service.FinishedRun{RunID: runs[0].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal(e)
	}
	open := ciIssue(t, p)
	scopedAdmin := actor.Actor{Identity: "scoped-admin", AuthMethod: "key", Fingerprint: "scoped-admin", Admin: true, Scope: []string{"origin"}}
	f.store.Actors[scopedAdmin.Fingerprint] = scopedAdmin
	f.store.Policies[scopedAdmin.Fingerprint] = service.KeyPolicy{Agent: true}
	for _, c := range []apicontract.Caller{workerCaller(), {Actor: scopedAdmin}} {
		e := closeIssue(t, p, c, apicontract.IssueCloseArgs{ID: open.ID, Reason: "not mine"})
		requireCode(t, e, publicerr.InvalidTransition)
		if ciIssue(t, p).State != model.IssueOpen {
			t.Fatal("an uplink closed a CI issue", c.Actor.Identity)
		}
	}
	for _, blank := range []string{"", "  "} {
		e := closeIssue(t, p, adminCaller(), apicontract.IssueCloseArgs{ID: open.ID, Reason: blank})
		requireField(t, e, "reason")
		if ciIssue(t, p).State != model.IssueOpen {
			t.Fatal("an administrator closed a CI issue without a reason")
		}
	}
	const reason = "created by branch CI in error"
	if e := closeIssue(t, p, adminCaller(), apicontract.IssueCloseArgs{ID: open.ID, Reason: reason}); e != nil {
		t.Fatal(e)
	}
	if v := ciIssue(t, p); v.State != model.IssueClosed {
		t.Fatal("administrator close did not close the CI issue", v)
	}
	assertCloseAudited(t, p, reason)
	if e := f.s.Finished(t.Context(), service.FinishedRun{RunID: runs[1].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal(e)
	}
	if v := ciIssue(t, p); v.ID != open.ID || v.State != model.IssueOpen || v.CIWorkID != runs[1].ID {
		t.Fatal("a later default branch failure did not reopen the issue", v)
	}
}

func closeIssue(t *testing.T, p *peopleFixture, c apicontract.Caller, q apicontract.IssueCloseArgs) error {
	t.Helper()
	call, e := p.f.s.PrepareIssueClose(t.Context(), c, q)
	_, e = execute(t.Context(), call, e)
	return e
}

func assertCloseAudited(t *testing.T, p *peopleFixture, reason string) {
	t.Helper()
	audits, e := p.state.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Action: "issue.close", Limit: 10})
	if e != nil || len(audits) != 1 || audits[0].Actor != admin().Identity || !strings.Contains(audits[0].Details, reason) {
		t.Fatal("administrator close reason not recorded", audits, e)
	}
}

func ciIssue(t *testing.T, p *peopleFixture) model.Issue {
	t.Helper()
	rows, e := p.state.Issues(t.Context(), nil, model.IssueFilter{Kind: model.IssueCI, Limit: 50})
	if e != nil || len(rows) != 1 {
		t.Fatal("want one CI issue", rows, e)
	}
	return rows[0]
}
