package service_test

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
)

func TestIssueListDefaultsToTwentyWithContinuation(t *testing.T) {
	t.Parallel()
	p := people(t)
	first := createTask(t, p, "origin", "first task")
	for range 20 {
		createTask(t, p, "origin", "another task")
	}
	call, err := p.f.s.PrepareIssueList(t.Context(), workerCaller(), apicontract.IssueListArgs{})
	page, err := execute(t.Context(), call, err)
	if err != nil || len(page.Issues) != 20 {
		t.Fatal("default page must contain twenty issues", len(page.Issues), err)
	}
	if page.NextBefore != page.Issues[19].ID || page.NextBefore == 0 {
		t.Fatal("default page lost its continuation", page.NextBefore)
	}
	call, err = p.f.s.PrepareIssueList(t.Context(), workerCaller(), apicontract.IssueListArgs{Before: page.NextBefore})
	last, err := execute(t.Context(), call, err)
	if err != nil || len(last.Issues) != 1 || last.Issues[0].ID != first.ID || last.NextBefore != 0 {
		t.Fatal("continuation did not return the remaining issue", last, err)
	}
}

func TestIssueStalenessUsesWorkStatusAndExactThreshold(t *testing.T) {
	t.Parallel()
	p := people(t)
	for _, test := range []struct {
		name   string
		status model.IssueWorkStatus
		age    time.Duration
		active string
		stale  bool
	}{
		{"todo before threshold", model.IssueTodo, time.Hour - time.Nanosecond, "2h", false},
		{"todo at threshold", model.IssueTodo, time.Hour, "2h", true},
		{"active before override", model.IssueActive, time.Hour, "2h", false},
		{"active at override", model.IssueActive, 2 * time.Hour, "2h", true},
		{"active with ordinary threshold", model.IssueActive, time.Hour, "", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p.f.offset.Store(0)
			v := createTask(t, p, "origin", "work status observation")
			update, err := p.f.s.PrepareIssueUpdate(t.Context(), workerCaller(), apicontract.IssueUpdateArgs{ID: v.ID, WorkStatus: new(string(test.status))})
			changed, err := execute(t.Context(), update, err)
			if err != nil {
				t.Fatal(err)
			}
			p.f.offset.Store(int64(test.age))
			read, err := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: v.ID, StaleAfter: "1h", ActiveStaleAfter: test.active})
			got, err := execute(t.Context(), read, err)
			if err != nil || got.Stale != test.stale {
				t.Fatal("wrong stale observation", got.Stale, err)
			}
			if got.Revision != changed.Issue.Revision || !got.UpdatedAt.Equal(changed.Issue.UpdatedAt) {
				t.Fatal("observation changed the issue revision or timestamp", got)
			}
		})
	}
}
