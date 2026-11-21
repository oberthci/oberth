package service_test

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

func listIssueIDs(t *testing.T, p *peopleFixture, q apicontract.IssueListArgs) (apicontract.IssuePage, []int64) {
	t.Helper()
	call, err := p.f.s.PrepareIssueList(t.Context(), workerCaller(), q)
	page, err := execute(t.Context(), call, err)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for _, v := range page.Issues {
		ids = append(ids, v.ID)
	}
	return page, ids
}

// A plain read uses the installation's windows; stale_after replaces the
// general window and, alone, the active one; active_stale_after replaces only
// the active one. No read changes the issue.
func TestIssueReadsUseInstallationWindowsUnlessNamed(t *testing.T) {
	t.Parallel()
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
			}
			p.f.c.People.IssueWindows = model.IssueWindows{General: 24 * time.Hour, Active: 2 * time.Hour}
			p.f.restart(t)
			todo := createTask(t, p, "origin", "waiting for someone")
			active := createTask(t, p, "origin", "claimed by an agent")
			update, err := p.f.s.PrepareIssueUpdate(t.Context(), workerCaller(), apicontract.IssueUpdateArgs{ID: active.ID, WorkStatus: new(string(model.IssueActive))})
			changed, err := execute(t.Context(), update, err)
			if err != nil {
				t.Fatal(err)
			}
			p.f.offset.Store(int64(3 * time.Hour))
			for _, test := range []windowCase{
				{"installation windows", "", "", false, true},
				{"stale_after replaces both", "1h", "", true, true},
				{"both named", "1h", "4h", true, false},
				{"only the active window named", "", "4h", false, false},
			} {
				checkWindowCase(t, p, test, [2]model.Issue{todo, *changed.Issue})
			}
			if _, ids := listIssueIDs(t, p, apicontract.IssueListArgs{Stale: true}); !slices.Equal(ids, []int64{active.ID}) {
				t.Fatal("stale filter under installation windows", ids)
			}
		})
	}
}

// windowCase reads a todo and an active issue, both three hours untouched.
type windowCase struct {
	name                 string
	stale, activeStale   string
	todoStale, activeOld bool
}

// checkWindowCase reads issues {todo, active} under one case's windows.
func checkWindowCase(t *testing.T, p *peopleFixture, test windowCase, issues [2]model.Issue) {
	t.Helper()
	todo, active := issues[0], issues[1]
	page, _ := listIssueIDs(t, p, apicontract.IssueListArgs{StaleAfter: test.stale, ActiveStaleAfter: test.activeStale})
	listed := map[int64]bool{}
	for _, v := range page.Issues {
		listed[v.ID] = v.Stale
	}
	want := len(slices.DeleteFunc([]bool{test.todoStale, test.activeOld}, func(stale bool) bool { return !stale }))
	if listed[todo.ID] != test.todoStale || listed[active.ID] != test.activeOld || page.Counts.Stale != want {
		t.Fatal(test.name, listed, page.Counts)
	}
	read, err := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: active.ID, StaleAfter: test.stale, ActiveStaleAfter: test.activeStale})
	got, err := execute(t.Context(), read, err)
	if err != nil || got.Stale != test.activeOld || got.Revision != active.Revision || !got.UpdatedAt.Equal(active.UpdatedAt) {
		t.Fatal(test.name, "issue_get disagrees or changed the issue", got, err)
	}
}

// Newer and older pages meet exactly, each reply places its page in the
// total, and a cursor is offered only toward issues that exist.
func TestIssueListPagesBothWaysWithPosition(t *testing.T) {
	t.Parallel()
	p := people(t)
	ids := []int64{}
	for range 5 {
		ids = append(ids, createTask(t, p, "origin", "queued work").ID)
	}
	slices.Reverse(ids)
	for _, test := range []struct {
		name         string
		q            apicontract.IssueListArgs
		want         []int64
		offset       int
		newer, older int64
	}{
		{"first", apicontract.IssueListArgs{Limit: 2}, ids[0:2], 0, 0, ids[1]},
		{"older", apicontract.IssueListArgs{Limit: 2, Before: ids[1]}, ids[2:4], 2, ids[2], ids[3]},
		{"oldest", apicontract.IssueListArgs{Limit: 2, Before: ids[3]}, ids[4:5], 4, ids[4], 0},
		{"newer from the oldest", apicontract.IssueListArgs{Limit: 2, After: ids[4]}, ids[2:4], 2, ids[2], ids[3]},
		{"newer reaches the first", apicontract.IssueListArgs{Limit: 2, After: ids[2]}, ids[0:2], 0, 0, ids[1]},
	} {
		page, got := listIssueIDs(t, p, test.q)
		if !slices.Equal(got, test.want) || page.Total != 5 || page.Offset != test.offset || page.NextAfter != test.newer || page.NextBefore != test.older {
			t.Fatalf("%s: ids %v offset %d newer %d older %d", test.name, got, page.Offset, page.NextAfter, page.NextBefore)
		}
	}
	_, err := p.f.s.PrepareIssueList(t.Context(), workerCaller(), apicontract.IssueListArgs{Before: ids[1], After: ids[3]})
	requireCode(t, err, publicerr.InvalidInput)
}
