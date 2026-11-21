package service_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

func TestIssueImportUnknownNameRefusalsStayBounded(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		t.Run(fmt.Sprint(oversized), func(t *testing.T) { checkBoundedUnknownNames(t, oversized) })
	}
}

func checkBoundedUnknownNames(t *testing.T, oversized bool) {
	t.Helper()
	p := people(t)
	calls := &countedIssueImports{IssueStore: p.f.c.People.Issues}
	p.f.c.People.Issues = calls
	p.f.restart(t)
	records := make([]model.IssueImportRecord, 300)
	for i := range records {
		records[i] = importRecord(40 + int64(i))
		records[i].Upstream = fmt.Sprintf("n%03d-", i) + strings.Repeat("a", 48)
		if oversized {
			records[i].Upstream = strings.Repeat("x", 2000) + fmt.Sprint(i)
		}
	}
	q := importArgs(t, records...)
	for _, dry := range []bool{true, false} {
		q.DryRun = dry
		_, err := importCall(t, p, q)
		assertBoundedUnknownNames(t, err, oversized, records)
	}
	if calls.calls != 0 {
		t.Fatal("unknown names reached persistence")
	}
}

func assertBoundedUnknownNames(t *testing.T, err error, oversized bool, records []model.IssueImportRecord) {
	t.Helper()
	if err == nil {
		t.Fatal("unknown names accepted")
	}
	problem := publicerr.Classify(err)
	context := strings.Join(problem.Context, " ")
	if len(context) > 600 || !strings.Contains(context, "line 2, issue 40") {
		t.Fatal("unbounded refusal or lost source position", len(context))
	}
	if oversized {
		if !strings.Contains(problem.Remedy(), "1..53") || strings.Contains(context, strings.Repeat("x", 100)) {
			t.Fatal("invalid source name was echoed or lost its bound", problem)
		}
	} else if !strings.Contains(context, "and 292 more") || !strings.Contains(context, records[0].Upstream) || strings.Contains(context, records[299].Upstream) {
		t.Fatal("unknown-name summary lost its count or first names", context)
	}
}

func TestIssueImportChangedMappingRemedyNamesTheSameOption(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) { checkMappingConflictRemedy(t, issueUpstreamPeople(t, real)) })
	}
}

func checkMappingConflictRemedy(t *testing.T, p *peopleFixture) {
	t.Helper()
	registerIssueUpstream(t, p, "old")
	registerIssueUpstream(t, p, "other")
	row := importRecord(40)
	row.Upstream, row.Repo = "old", "old/team/widget"
	q := importArgs(t, row)
	q.Upstream = "origin"
	if _, err := importCall(t, p, q); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"other", ""} {
		for _, dry := range []bool{false, true} {
			q.Upstream, q.DryRun = target, dry
			_, err := importCall(t, p, q)
			if err == nil {
				t.Fatal("changed mapping was replayed")
			}
			problem := publicerr.Classify(err)
			if problem.Code() != publicerr.Conflict || !strings.Contains(problem.Remedy(), "with the same --upstream") || len(problem.Remedy()) > 120 {
				t.Fatal("mapping conflict remedy is incomplete", problem)
			}
		}
	}
	got, err := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, row.ID)
	if err != nil || got.Upstream != "origin" {
		t.Fatal("conflicting mapping changed the issue", err)
	}
}

func TestIssueDefaultChoicesMatchScopedRepositoryDiscovery(t *testing.T) {
	p := people(t)
	registerIssueUpstream(t, p, "private")
	registerIssueUpstream(t, p, "hidden")
	checkScopedIssueChoices(t, p, []string{"origin", "private"})
	checkScopedIssueChoices(t, p, []string{"private"})
}

func checkScopedIssueChoices(t *testing.T, p *peopleFixture, scope []string) {
	t.Helper()
	a := caller()
	a.Scope = scope
	p.f.store.Actors[a.Fingerprint] = a
	c := apicontract.Caller{Actor: a}
	list, err := p.f.s.PrepareRepoList(t.Context(), c, apicontract.RepoListArgs{})
	page, err := execute(t.Context(), list, err)
	if err != nil {
		t.Fatal(err)
	}
	call, err := p.f.s.PrepareIssueCreate(t.Context(), c, apicontract.IssueCreateArgs{Title: "task", Body: new("body")})
	issue, err := execute(t.Context(), call, err)
	if len(page.Upstreams) == 1 {
		if err != nil || issue.Issue.Upstream != page.Upstreams[0].Name {
			t.Fatal("default differs from scoped repository discovery", err)
		}
		return
	}
	if err == nil {
		t.Fatal("ambiguous scoped choices created an issue")
	}
	names := make([]string, 0, len(page.Upstreams))
	for _, upstream := range page.Upstreams {
		names = append(names, fmt.Sprintf("%q", upstream.Name))
	}
	slices.Sort(names)
	context := strings.Join(publicerr.Classify(err).Context, " ")
	if !strings.Contains(context, "Upstream choices: "+strings.Join(names, ", ")) || strings.Contains(context, "hidden") {
		t.Fatal("issue choices differ from scoped discovery", context)
	}
}

func TestIssueEmptyUpstreamEditRefusesDuringPreparation(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := issueUpstreamPeople(t, real)
			before := createTask(t, p, "origin", "body")
			for _, name := range []string{"", "global"} {
				call, err := p.f.s.PrepareIssueUpdate(t.Context(), adminCaller(), apicontract.IssueUpdateArgs{ID: before.ID, Upstream: &name})
				if err == nil || call != nil || publicerr.Classify(err).Reason() != "Invalid field: upstream." {
					t.Fatal("invalid edit did not stop at name admission", name, err)
				}
			}
			got, err := p.f.c.People.Issues.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, before.ID)
			if err != nil || got.Upstream != before.Upstream || got.Revision != before.Revision {
				t.Fatal("refused name changed the issue", err)
			}
		})
	}
}
