package service_test

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestScopedIssueEventsSurviveDeletion(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			checkScopedIssueEvents(t, real)
		})
	}
}

func checkScopedIssueEvents(t *testing.T, real bool) {
	p := people(t)
	p.f.c.Events = p.state
	if !real {
		p.f.c.People.Issues = p.f.store.native
		p.f.c.Events = p.f.store.native
	}
	p.f.restart(t)
	registerIssueUpstream(t, p, "private")
	issue := createTask(t, p, "origin", "body")
	createTask(t, p, "private", "hidden")
	if _, err := p.f.c.People.Issues.CreateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.Issue{Kind: model.IssueManual, Title: "installation task"}); err != nil {
		t.Fatal(err)
	}
	changeIssueForEvents(t, p, issue.ID)
	p.f.store.native.SeedRepositories(nil)
	wait, err := p.f.s.PrepareEventsWait(t.Context(), workerCaller(), apicontract.EventsWaitArgs{AfterID: new(int64(0)), Kinds: []string{"issue.updated"}, TimeoutS: 1})
	page, err := execute(t.Context(), wait, err)
	if err != nil || len(page.Events) != 5 {
		t.Fatal(page, err)
	}
	for _, e := range page.Events {
		if e.Kind != string(model.EventIssueUpdated) || e.Upstream != "origin" || e.IssueID != issue.ID || e.Repo != "" {
			t.Fatal("wrong issue event scope", e)
		}
	}
}

func changeIssueForEvents(t *testing.T, p *peopleFixture, id int64) {
	update, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: id, Title: new("edited")})
	if _, err = execute(t.Context(), update, err); err != nil {
		t.Fatal(err)
	}
	closeCall, err := p.f.s.PrepareIssueClose(t.Context(), adminCaller(), apicontract.IssueCloseArgs{ID: id})
	if _, err = execute(t.Context(), closeCall, err); err != nil {
		t.Fatal(err)
	}
	reopen, err := p.f.s.PrepareIssueReopen(t.Context(), adminCaller(), apicontract.IssueReopenArgs{ID: id})
	if _, err = execute(t.Context(), reopen, err); err != nil {
		t.Fatal(err)
	}
	del, err := p.f.s.PrepareIssueDelete(t.Context(), adminCaller(), apicontract.IssueDeleteArgs{ID: id})
	if _, err = execute(t.Context(), del, err); err != nil {
		t.Fatal(err)
	}
}
