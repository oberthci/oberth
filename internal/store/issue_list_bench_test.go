package store

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// BenchmarkIssueListTwoThousand measures one issue_list read, the page of 50
// and its totals, over 2,000 visible issues with 1 KiB bodies.
func BenchmarkIssueListTwoThousand(b *testing.B) {
	s, err := Create(b.Context(), filepath.Join(b.TempDir(), "state.sqlite"), testOptions())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	now := testOptions().Clock()
	statuses := []model.IssueWorkStatus{model.IssueTodo, model.IssueActive, model.IssueWaiting, model.IssueReview}
	records := []model.IssueImportRecord{}
	for id := int64(1); id <= 2000; id++ {
		at := now.Add(-time.Duration(id) * time.Hour / 10)
		v := model.IssueImportRecord{ID: id, Upstream: "origin", Repo: "origin/team/repo", Title: "task", Body: strings.Repeat("b", 1024),
			State: model.IssueOpen, CreatedAt: at, UpdatedAt: at, Author: "source", Kind: model.IssueManual, WorkStatus: statuses[id%4], Occurrences: 1}
		if id%5 == 0 {
			v.State, v.ClosedAt = model.IssueClosed, &at
		}
		records = append(records, v)
	}
	if _, err := s.ImportIssues(b.Context(), importAdmin(), stateport.IssueImport{Records: records, FileSHA256: strings.Repeat("d", 64)}); err != nil {
		b.Fatal(err)
	}
	scope := stateport.IssueScope{Upstreams: []string{"origin"}}
	filter := model.IssueFilter{State: "open", Limit: 50, Before: 1000, Windows: model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}}
	for b.Loop() {
		page, err := s.ScopedIssues(b.Context(), scope, filter)
		if err != nil || len(page.Issues) != 50 || page.Counts.Open != 1600 {
			b.Fatal(err, len(page.Issues), page.Counts)
		}
	}
}
