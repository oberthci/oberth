package service_test

import (
	"context"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
	"testing"
)

func TestScopedAdministratorCannotReadGlobalIssue(t *testing.T) {
	t.Parallel()
	p := people(t)
	global, err := p.f.c.People.Issues.CreateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.Issue{Kind: model.IssueManual, Title: "installation"})
	if err != nil {
		t.Fatal(err)
	}
	own := createTask(t, p, "origin", "own")
	a := admin()
	a.Scope = []string{"origin"}
	p.f.store.Actors[a.Fingerprint] = a
	c := apicontract.Caller{Actor: a}
	call, e := p.f.s.PrepareIssueGet(t.Context(), c, apicontract.IssueGetArgs{ID: global.ID})
	requireCode(t, e, publicerr.NotFound)
	if call != nil {
		t.Fatal("hidden global issue prepared")
	}
	list, e := p.f.s.PrepareIssueList(t.Context(), c, apicontract.IssueListArgs{})
	page, e := execute(t.Context(), list, e)
	if e != nil || len(page.Issues) != 1 || page.Issues[0].ID != own.ID {
		t.Fatal("scoped admin saw global issue", page, e)
	}
}
func TestScopedKeyCannotPrepareForeignIssue(t *testing.T) {
	t.Parallel()
	p := people(t)
	call, e := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "other", Title: "task", Body: new("body")})
	requireCode(t, e, publicerr.NotFound)
	if call != nil {
		t.Fatal("foreign create prepared")
	}
}
func TestNonAdministratorCannotPrepareIssueMoveWithinScope(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := createTask(t, p, "origin", "body")
	a := caller()
	a.Scope = []string{"origin", "other"}
	p.f.store.Actors[a.Fingerprint] = a
	call, e := p.f.s.PrepareIssueUpdate(t.Context(), apicontract.Caller{Actor: a}, apicontract.IssueUpdateArgs{ID: v.ID, Upstream: new("other")})
	requireCode(t, e, publicerr.Forbidden)
	if call != nil {
		t.Fatal("upstream move prepared")
	}
}
func TestIssueCreateRequiresNonemptyTitle(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, title := range []string{"", " \t "} {
		call, e := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "origin", Title: title, Body: new("valid body")})
		requireField(t, e, "title/body")
		if call != nil {
			t.Fatal("empty title prepared")
		}
	}
}

type interleavedIssueRead struct {
	service.IssueStore
	after func() error
}

func (p *interleavedIssueRead) ScopedIssue(ctx context.Context, scope stateport.IssueScope, id int64) (model.Issue, error) {
	v, e := p.IssueStore.ScopedIssue(ctx, scope, id)
	if e != nil {
		return v, e
	}
	if p.after != nil {
		f := p.after
		p.after = nil
		e = f()
	}
	return v, e
}
func TestAppendCannotOverwriteInterleavedIssueEdit(t *testing.T) {
	t.Parallel()
	p := people(t)
	v := createTask(t, p, "origin", "original")
	p.f.c.People.Issues = &interleavedIssueRead{IssueStore: p.state, after: func() error {
		_, e := p.state.UpdateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.IssuePatch{ID: v.ID, Body: new("competing body")})
		return e
	}}
	p.f.restart(t)
	call, e := p.f.s.PrepareIssueAppend(t.Context(), workerCaller(), apicontract.IssueAppendArgs{ID: v.ID, Text: " appended"})
	_, e = execute(t.Context(), call, e)
	requireCode(t, e, publicerr.Conflict)
	got, e := p.state.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, v.ID)
	if e != nil || got.Body != "competing body" {
		t.Fatal("append lost concurrent edit", got, e)
	}
}
func TestPeopleToolsRequireConfiguredPeoplePorts(t *testing.T) {
	t.Parallel()
	f := setup(t)
	call, e := f.s.PrepareAdminVersion(t.Context(), adminCaller(), apicontract.AdminVersionArgs{})
	requireCode(t, e, publicerr.Unavailable)
	if call != nil {
		t.Fatal("unconfigured administration prepared")
	}
}
