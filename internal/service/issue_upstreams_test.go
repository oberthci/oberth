package service_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/stateport"
)

func issueUpstreamPeople(t *testing.T, real bool) *peopleFixture {
	t.Helper()
	p := people(t)
	if !real {
		p.f.c.People.Issues = p.f.store.native
		p.f.restart(t)
	}
	return p
}

func registerIssueUpstream(t *testing.T, p *peopleFixture, name string) {
	t.Helper()
	if err := p.state.RegisterUpstream(t.Context(), stateport.Action{Actor: admin().Identity}, stateport.Upstream{Name: name, Org: name + "-team", Kind: "git", BaseURL: "https://" + name + ".oberth.ci", KeyName: name + "-source"}); err != nil {
		t.Fatal(err)
	}
}

type countedIssueImports struct {
	service.IssueStore
	calls int
}

func (s *countedIssueImports) ImportIssues(ctx context.Context, a stateport.Action, q stateport.IssueImport) (stateport.IssueImportResult, error) {
	s.calls++
	return s.IssueStore.ImportIssues(ctx, a, q)
}

func TestIssueImportUnknownUpstreamsRefuseBeforeStoreAndDryRun(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := issueUpstreamPeople(t, real)
			calls := &countedIssueImports{IssueStore: p.f.c.People.Issues}
			p.f.c.People.Issues = calls
			p.f.restart(t)
			rows := []model.IssueImportRecord{importRecord(40), importRecord(41), importRecord(42), importRecord(43)}
			rows[1].Upstream, rows[2].Upstream, rows[3].Upstream = "zeta", "alpha", "zeta"
			q := importArgs(t, rows...)
			for _, dry := range []bool{true, false} {
				q.DryRun = dry
				_, err := importCall(t, p, q)
				assertUnknownUpstreams(t, err, `Unknown upstreams: "alpha", "zeta"`)
				context := strings.Join(publicerr.Classify(err).Context, " ")
				if !strings.Contains(context, "No issues imported.") || !strings.Contains(context, "line 3, issue 41") {
					t.Fatal("missing definite import refusal or source position", err)
				}
			}
			if calls.calls != 0 {
				t.Fatal("unknown names reached import persistence", calls.calls)
			}
			if created := createTask(t, p, "origin", "native"); created.ID != 1 {
				t.Fatal("refusal advanced allocation", created.ID)
			}
		})
	}
}

func assertUnknownUpstreams(t *testing.T, err error, names string) {
	t.Helper()
	if err == nil {
		t.Fatal("unregistered upstream accepted")
	}
	p := publicerr.Classify(err)
	requireCode(t, err, publicerr.InvalidInput)
	if !strings.Contains(strings.Join(p.Context, " "), names) || !strings.Contains(p.Remedy(), "repo add --upstream <name>") || !strings.Contains(p.Remedy(), "--upstream <local>") {
		t.Fatal("unknown-name diagnostic lacks names or remedy", p)
	}
}

func TestIssueImportMapsEveryUpstreamAndReplaysWithoutReplacingEdits(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) { checkIssueImportMapping(t, issueUpstreamPeople(t, real)) })
	}
}

func checkIssueImportMapping(t *testing.T, p *peopleFixture) {
	t.Helper()
	first, second := importRecord(40), importRecord(90)
	first.Upstream, first.Repo = "old-a", "old-a/team/widget"
	second.Upstream, second.Repo = "old-b", "old-b/team/another"
	q := importArgs(t, first, second)
	q.Upstream = "missing-local"
	_, err := importCall(t, p, q)
	assertUnknownUpstreams(t, err, `Unknown upstreams: "missing-local"`)
	q.Upstream, q.DryRun = "origin", true
	if _, err = importCall(t, p, q); err != nil {
		t.Fatal(err)
	}
	q.DryRun = false
	if _, err = importCall(t, p, q); err != nil {
		t.Fatal(err)
	}
	first.Upstream, first.Repo = "origin", "origin/team/widget"
	second.Upstream, second.Repo = "origin", "origin/team/another"
	checkImportedRead(t, p, first)
	checkImportedRead(t, p, second)
	edit, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: first.ID, Title: new("local edit")})
	if _, err = execute(t.Context(), edit, err); err != nil {
		t.Fatal(err)
	}
	replay, err := importCall(t, p, q)
	if err != nil || !replay.AlreadyImported {
		t.Fatal("mapped replay lost identity", replay, err)
	}
	read, err := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: first.ID})
	got, err := execute(t.Context(), read, err)
	if err != nil || got.Title != "local edit" {
		t.Fatal("mapped replay replaced a local edit or hid the issue", err)
	}
	registerIssueUpstream(t, p, "other")
	q.Upstream = "other"
	if _, err = importCall(t, p, q); err == nil {
		t.Fatal("a different mapping replayed the same IDs")
	}
}

func TestIssueImportMissingUpstreamNeedsExplicitMapping(t *testing.T) {
	p := people(t)
	row := importRecord(40)
	row.Upstream = ""
	q := importArgs(t, row)
	_, err := importCall(t, p, q)
	if err == nil || !strings.Contains(publicerr.Classify(err).Remedy(), "1..53") {
		t.Fatal("empty source upstream was not refused with its bound", err)
	}
	q.Upstream = "origin"
	if _, err = importCall(t, p, q); err != nil {
		t.Fatal(err)
	}
}

func TestIssueCreateRequiresARegisteredChoice(t *testing.T) {
	for _, mode := range []string{"none", "several", "unknown"} {
		t.Run(mode, func(t *testing.T) {
			p := people(t)
			q := apicontract.IssueCreateArgs{Title: "task", Body: new("body")}
			switch mode {
			case "none":
				p.f.c.People.Admin = emptyIssueUpstreams{AdminStore: p.state}
				p.f.restart(t)
			case "several":
				registerIssueUpstream(t, p, "private")
			case "unknown":
				q.Upstream = "missing"
			}
			call, err := p.f.s.PrepareIssueCreate(t.Context(), adminCaller(), q)
			_, err = execute(t.Context(), call, err)
			if err == nil {
				t.Fatal("created an issue without a registered choice")
			}
			text := strings.Join(publicerr.Classify(err).Context, " ")
			switch mode {
			case "none":
				if !strings.Contains(text, "(none)") {
					t.Fatal(text)
				}
			case "several":
				if !strings.Contains(text, `"origin", "private"`) {
					t.Fatal(text)
				}
			case "unknown":
				assertUnknownUpstreams(t, err, `Unknown upstreams: "missing"`)
			}
		})
	}
}

type emptyIssueUpstreams struct{ service.AdminStore }

func (emptyIssueUpstreams) UpstreamBindings(context.Context) ([]stateport.Upstream, error) {
	return []stateport.Upstream{}, nil
}

func TestIssueImportChecksLiveUpstreamsBeforeWriting(t *testing.T) {
	p := people(t)
	registerIssueUpstream(t, p, "departed")
	calls := &countedIssueImports{IssueStore: p.f.c.People.Issues}
	p.f.c.People.Issues = calls
	p.f.restart(t)
	row := importRecord(40)
	row.Upstream = "departed"
	call, err := p.f.s.PrepareIssueImport(t.Context(), adminCaller(), importArgs(t, row))
	if err != nil {
		t.Fatal(err)
	}
	if err := p.state.RemoveUpstream(t.Context(), stateport.Action{Actor: admin().Identity}, "departed"); err != nil {
		t.Fatal(err)
	}
	_, err = call(t.Context())
	assertUnknownUpstreams(t, err, `Unknown upstreams: "departed"`)
	if calls.calls != 0 {
		t.Fatal("stale upstream admission reached persistence")
	}
}

func TestIssueUpstreamEditsRejectUnregisteredNames(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := issueUpstreamPeople(t, real)
			before := createTask(t, p, "origin", "body")
			for _, name := range []string{"missing"} {
				call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: before.ID, Upstream: &name})
				_, err = execute(t.Context(), call, err)
				assertUnknownUpstreams(t, err, "Unknown upstreams: "+fmt.Sprintf("%q", name))
			}
			got, err := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, before.ID)
			if err != nil || got.Upstream != before.Upstream || got.Revision != before.Revision {
				t.Fatal("refused upstream edit changed the issue", err)
			}
			registerIssueUpstream(t, p, "other")
			call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: before.ID, Upstream: new("other")})
			changed, err := execute(t.Context(), call, err)
			if err != nil || changed.Issue.Upstream != "other" {
				t.Fatal("registered upstream edit failed", err)
			}
		})
	}
}
