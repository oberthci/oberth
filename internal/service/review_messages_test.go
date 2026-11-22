package service_test

import (
	"errors"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stepmodel"
	"strings"
	"testing"
)

type forbiddenRandom struct{}

func (forbiddenRandom) Read([]byte) (int, error) {
	return 0, errors.New("unexpected discarded bearer generation")
}
func TestRedemptionCreatesOnlyTheReturnedCertificate(t *testing.T) {
	t.Parallel()
	p := people(t)
	v, q := reviewInvitation(t, p)
	p.f.c.People.Random = forbiddenRandom{}
	p.f.restart(t)
	got, e := p.f.s.Redeem(t.Context(), v.ID, q)
	if e != nil || got.Certificate == "" {
		t.Fatal("redemption minted an unused bearer", got, e)
	}
}
func TestRefusalRemediesNameInvitationAndIssue(t *testing.T) {
	t.Parallel()
	p := people(t)
	v, q := reviewInvitation(t, p)
	q.Secret = strings.Repeat("x", 48)
	_, e := p.f.s.Redeem(t.Context(), v.ID, q)
	if !strings.Contains(publicerr.Classify(e).Error(), "new invitation") {
		t.Fatal("invitation remedy", e)
	}
	_, e = p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: 9999})
	if !strings.Contains(publicerr.Classify(e).Error(), "visible issue") {
		t.Fatal("issue remedy", e)
	}
	f := p.f
	f.c.Runs, f.c.Catalog, f.c.Events = p.state, p.state, p.state
	f.restart(t)
	runs := acceptCISequence(t, p)
	if e = f.s.Finished(t.Context(), service.FinishedRun{RunID: runs[0].ID, Status: stepmodel.RunFailed}); e != nil {
		t.Fatal(e)
	}
	issues, e := p.state.Issues(t.Context(), nil, model.IssueFilter{Limit: 20})
	if e != nil || len(issues) != 1 {
		t.Fatal(issues, e)
	}
	call, e := f.s.PrepareIssueClose(t.Context(), workerCaller(), apicontract.IssueCloseArgs{ID: issues[0].ID})
	_, e = execute(t.Context(), call, e)
	if !strings.Contains(publicerr.Classify(e).Error(), "Pass a later scheduled or default-branch run, or ask an administrator to close it.") {
		t.Fatal("CI issue remedy", e)
	}
}
func TestAdministrationRecordsAuditWithoutEvents(t *testing.T) {
	t.Parallel()
	p := people(t)
	p.f.c.Events = p.state
	p.f.restart(t)
	call, e := p.f.s.PrepareAdminAuditRecord(t.Context(), adminCaller(), apicontract.AdminAuditRecordArgs{Action: "upgrade.completed"})
	if _, e = execute(t.Context(), call, e); e != nil {
		t.Fatal(e)
	}
	audits, _, e := p.state.Audit(t.Context(), 0, 200)
	if e != nil || len(audits) == 0 || audits[len(audits)-1].Action != "upgrade.completed" {
		t.Fatal("administrative audit missing", audits, e)
	}
	for _, a := range []actor.Actor{caller(), admin()} {
		page, e := p.f.s.EventsSince(t.Context(), a, eventsInput{})
		if e != nil || len(page.Events) != 0 || page.LatestID != 0 {
			t.Fatal("administration generated an event", page, e)
		}
	}
}
