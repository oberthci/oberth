package contracttest

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

// listedIssue is one imported record; its age at the read decides staleness
// under the 24h general and 2h active windows of listWindows.
type listedIssue struct {
	id       int64
	upstream string
	repo     string
	state    model.IssueState
	status   model.IssueWorkStatus
	age      time.Duration
	title    string
}

func listedIssues() []listedIssue {
	return []listedIssue{
		{1, "origin", administrativeRepo, model.IssueOpen, model.IssueTodo, 30 * time.Hour, "old task"},
		{2, "origin", administrativeRepo, model.IssueOpen, model.IssueTodo, time.Hour, "fresh task"},
		{3, "origin", administrativeRepo, model.IssueOpen, model.IssueActive, 3 * time.Hour, "silent needle"},
		{4, "origin", administrativeRepo, model.IssueOpen, model.IssueActive, time.Hour, "busy task"},
		{5, "origin", "", model.IssueOpen, model.IssueWaiting, 25 * time.Hour, "waiting task"},
		{6, "origin", "", model.IssueOpen, model.IssueReview, 24 * time.Hour, "review at the window"},
		{7, "origin", "", model.IssueClosed, model.IssueTodo, 48 * time.Hour, "closed task"},
		{8, "other", "", model.IssueOpen, model.IssueTodo, 30 * time.Hour, "foreign needle"},
		{9, "other", "", model.IssueClosed, model.IssueReview, time.Hour, "foreign closed"},
		{10, "origin", "", model.IssueOpen, model.IssueTodo, 24*time.Hour - time.Millisecond, "just inside the window"},
	}
}

func listWindows() model.IssueWindows {
	return model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}
}

// importListedIssues saves the records with source timestamps in another
// zone, so the store compares instants rather than text.
func importListedIssues(t *testing.T, f AdministrationFixture) {
	t.Helper()
	now := f.Now()
	records := []model.IssueImportRecord{}
	for _, v := range listedIssues() {
		at := now.Add(-v.age).In(time.FixedZone("source", 2*3600))
		record := model.IssueImportRecord{ID: v.id, Upstream: v.upstream, Repo: v.repo, Title: v.title, State: v.state, CreatedAt: at, UpdatedAt: at,
			Author: "source author", Kind: model.IssueManual, WorkStatus: v.status, Occurrences: 1}
		if v.state == model.IssueClosed {
			record.ClosedAt = &at
		}
		records = append(records, record)
	}
	admin := stateport.Action{Actor: "administrator", Caller: actor.Actor{Identity: "administrator", Admin: true}}
	_, err := f.Port.ImportIssues(t.Context(), admin, stateport.IssueImport{Records: records, FileSHA256: strings.Repeat("c", 64)})
	mustRun(t, err)
}

func listIssues(t *testing.T, f AdministrationFixture, upstream string, q model.IssueFilter) stateport.IssuePage {
	t.Helper()
	if q.Limit == 0 {
		q.Limit = 3
	}
	q.Windows = listWindows()
	page, err := f.Port.ScopedIssues(t.Context(), stateport.IssueScope{Upstreams: []string{upstream}}, q)
	mustRun(t, err)
	return page
}

func pageIDs(page stateport.IssuePage) []int64 {
	ids := []int64{}
	for _, v := range page.Issues {
		ids = append(ids, v.ID)
	}
	return ids
}

func checkIssueListCounts(t *testing.T, f AdministrationFixture) {
	importListedIssues(t, f)
	all := model.IssueCounts{Open: 7, Closed: 1, Todo: 3, Active: 2, Waiting: 1, Review: 1, Stale: 4}
	for _, test := range []struct {
		name     string
		upstream string
		filter   model.IssueFilter
		counts   model.IssueCounts
	}{
		{"scope only", "origin", model.IssueFilter{}, all},
		{"other scope never sees origin", "other", model.IssueFilter{}, model.IssueCounts{Open: 1, Closed: 1, Todo: 1, Stale: 1}},
		{"repository", "origin", model.IssueFilter{Repo: administrativeRepo}, model.IssueCounts{Open: 4, Todo: 2, Active: 2, Stale: 2}},
		{"query", "origin", model.IssueFilter{Query: "NEEDLE"}, model.IssueCounts{Open: 1, Active: 1, Stale: 1}},
		{"state, status and stale choices do not narrow counts", "origin", model.IssueFilter{State: "closed", WorkStatus: model.IssueReview, Stale: true}, all},
	} {
		if got := listIssues(t, f, test.upstream, test.filter).Counts; got != test.counts {
			t.Errorf("%s: counts %+v, want %+v", test.name, got, test.counts)
		}
	}
	page := listIssues(t, f, "origin", model.IssueFilter{State: "open", Limit: 10})
	stale := []int64{}
	for _, v := range page.Issues {
		if v.Stale {
			stale = append(stale, v.ID)
		}
	}
	if !slices.Equal(stale, []int64{6, 5, 3, 1}) || page.Total != 7 {
		t.Fatal("row staleness disagrees with the counts", stale, page.Total)
	}
	page = listIssues(t, f, "origin", model.IssueFilter{State: "open", Stale: true, Limit: 10})
	if !slices.Equal(pageIDs(page), []int64{6, 5, 3, 1}) || page.Total != 4 || page.Offset != 0 {
		t.Fatal("stale filter", pageIDs(page), page.Total, page.Offset)
	}
	plain, err := f.Port.ScopedIssues(t.Context(), stateport.IssueScope{Upstreams: []string{"origin"}}, model.IssueFilter{State: "open", Limit: 10})
	mustRun(t, err)
	if plain.Counts.Stale != 0 || slices.ContainsFunc(plain.Issues, func(v model.Issue) bool { return v.Stale }) {
		t.Fatal("zero windows marked issues stale", plain.Counts)
	}
}

func checkIssueListPaging(t *testing.T, f AdministrationFixture) {
	importListedIssues(t, f)
	for _, test := range []struct {
		name          string
		filter        model.IssueFilter
		ids           []int64
		total, offset int
	}{
		{"first page", model.IssueFilter{State: "open"}, []int64{10, 6, 5}, 7, 0},
		{"older", model.IssueFilter{State: "open", Before: 5}, []int64{4, 3, 2}, 7, 3},
		{"oldest", model.IssueFilter{State: "open", Before: 2}, []int64{1}, 7, 6},
		{"newer from the oldest", model.IssueFilter{State: "open", After: 1}, []int64{4, 3, 2}, 7, 3},
		{"newer from the middle", model.IssueFilter{State: "open", After: 4}, []int64{10, 6, 5}, 7, 0},
		{"newer reaches the newest", model.IssueFilter{State: "open", After: 5}, []int64{10, 6}, 7, 0},
		{"past the oldest", model.IssueFilter{State: "open", Before: 1}, []int64{}, 7, 7},
		{"past the newest", model.IssueFilter{State: "open", After: 10}, []int64{}, 7, 0},
		{"work status", model.IssueFilter{State: "open", WorkStatus: model.IssueActive}, []int64{4, 3}, 2, 0},
		{"all states", model.IssueFilter{State: "all", Before: 7}, []int64{6, 5, 4}, 8, 2},
	} {
		page := listIssues(t, f, "origin", test.filter)
		if !slices.Equal(pageIDs(page), test.ids) || page.Total != test.total || page.Offset != test.offset {
			t.Errorf("%s: ids %v total %d offset %d, want %v %d %d", test.name, pageIDs(page), page.Total, page.Offset, test.ids, test.total, test.offset)
		}
	}
	if _, err := f.Port.ScopedIssues(t.Context(), stateport.IssueScope{Upstreams: []string{"origin"}}, model.IssueFilter{Limit: 3, Before: 5, After: 1}); err == nil {
		t.Fatal("both cursors accepted")
	}
}
