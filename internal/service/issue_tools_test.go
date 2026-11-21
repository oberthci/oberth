package service_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
)

// Both implementations run the same tool-level scope/CAS/lock/budget contract.
func TestScopedIssueToolContract(t *testing.T) {
	t.Parallel()
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			issueScopeReads(t, p)
			issueCASAndLock(t, p)
			issueReadBudgets(t, p)
		})
	}
}
func createTask(t *testing.T, p *peopleFixture, upstream, body string) model.Issue {
	t.Helper()
	call, e := p.f.s.PrepareIssueCreate(t.Context(), adminCaller(), apicontract.IssueCreateArgs{Upstream: upstream, Title: "task", Body: &body})
	v, e := execute(t.Context(), call, e)
	if e != nil {
		t.Fatal(e)
	}
	return *v.Issue
}
func issueScopeReads(t *testing.T, p *peopleFixture) {
	registerIssueUpstream(t, p, "private")
	own := createTask(t, p, "origin", "complete body")
	hidden := createTask(t, p, "private", "secret issue")
	global, e := p.f.c.People.Issues.CreateIssue(t.Context(), stateport.Action{Actor: admin().Identity}, model.Issue{Kind: model.IssueManual, Title: "installation task"})
	if e != nil {
		t.Fatal(e)
	}
	for _, id := range []int64{hidden.ID, global.ID} {
		if _, e := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: id}); e == nil {
			t.Fatal("hidden issue prepared", id)
		} else {
			requireCode(t, e, publicerr.NotFound)
		}
	}
	call, e := p.f.s.PrepareIssueList(t.Context(), workerCaller(), apicontract.IssueListArgs{Limit: 1})
	page, e := execute(t.Context(), call, e)
	if e != nil || len(page.Issues) != 1 || page.Issues[0].ID != own.ID || page.Issues[0].Body != "" || page.NextBefore != 0 || page.NextAfter != 0 {
		t.Fatal("scope was applied after paging or bodies returned", page, e)
	}
	assertScopedIssueCounts(t, p, page)
	assertIssueManyOrder(t, p, []int64{hidden.ID, own.ID, global.ID, own.ID})
	assertScopedIssueCreate(t, p)
	if _, e = p.f.s.PrepareIssueUpdate(t.Context(), workerCaller(), apicontract.IssueUpdateArgs{ID: own.ID, Upstream: new("private")}); e == nil {
		t.Fatal("uplink moved issue scope")
	}
}

// The uplink limited to origin counts only origin's issue: neither the
// private upstream's nor the installation-wide one; the administrator all three.
func assertScopedIssueCounts(t *testing.T, p *peopleFixture, uplink apicontract.IssuePage) {
	t.Helper()
	if uplink.Counts != (model.IssueCounts{Open: 1, Todo: 1}) || uplink.Total != 1 || uplink.Offset != 0 {
		t.Fatal("counts reached beyond the caller's scope", uplink.Counts, uplink.Total, uplink.Offset)
	}
	call, e := p.f.s.PrepareIssueList(t.Context(), adminCaller(), apicontract.IssueListArgs{Limit: 1})
	page, e := execute(t.Context(), call, e)
	if e != nil || page.Counts.Open != 3 || page.Total != 3 || page.NextBefore != page.Issues[0].ID {
		t.Fatal("installation administrator does not count every upstream", page.Counts, page.Total, e)
	}
}
func issueCASAndLock(t *testing.T, p *peopleFixture) {
	v := createTask(t, p, "origin", "original")
	ctx := t.Context()
	call, e := p.f.s.PrepareIssueLock(ctx, workerCaller(), apicontract.IssueLockArgs{ID: v.ID})
	lock, e := execute(ctx, call, e)
	if e != nil {
		t.Fatal(e)
	}
	assertIssueReadDoesNotExtend(t, p, v, lock)
	update, e := p.f.s.PrepareIssueUpdate(ctx, adminCaller(), apicontract.IssueUpdateArgs{ID: v.ID, Title: new("blocked")})
	if _, e = execute(ctx, update, e); e == nil {
		t.Fatal("foreign lock bypassed")
	}
	appendCall, e := p.f.s.PrepareIssueAppend(ctx, workerCaller(), apicontract.IssueAppendArgs{ID: v.ID, Text: " + note"})
	changed, e := execute(ctx, appendCall, e)
	if e != nil || changed.Issue.Body != "original + note" || changed.AppendedBytes != 7 {
		t.Fatal(changed, e)
	}
	update, e = p.f.s.PrepareIssueUpdate(ctx, workerCaller(), apicontract.IssueUpdateArgs{ID: v.ID, ExpectedRevision: &v.Revision, Title: new("stale")})
	if _, e = execute(ctx, update, e); e == nil {
		t.Fatal("stale revision accepted")
	}
	p.f.offset.Add(int64(6 * time.Minute))
	read, e := p.f.s.PrepareIssueGet(ctx, workerCaller(), apicontract.IssueGetArgs{ID: v.ID, StaleAfter: "1m"})
	got, e := execute(ctx, read, e)
	if e != nil || got.Lock != nil || !got.Stale {
		t.Fatal("stale/expired observation wrong", got, e)
	}
	issueManualLifecycle(t, p, v.ID)
}
func issueManualLifecycle(t *testing.T, p *peopleFixture, id int64) {
	ctx := t.Context()
	closeCall, e := p.f.s.PrepareIssueClose(ctx, workerCaller(), apicontract.IssueCloseArgs{ID: id})
	closed, e := execute(ctx, closeCall, e)
	if e != nil || closed.State != model.IssueClosed {
		t.Fatal(closed, e)
	}
	reopen, e := p.f.s.PrepareIssueReopen(ctx, workerCaller(), apicontract.IssueReopenArgs{ID: id})
	opened, e := execute(ctx, reopen, e)
	if e != nil || opened.State != model.IssueOpen {
		t.Fatal(opened, e)
	}
	del, e := p.f.s.PrepareIssueDelete(ctx, adminCaller(), apicontract.IssueDeleteArgs{ID: id})
	deleted, e := execute(ctx, del, e)
	if e != nil || !deleted.Deleted {
		t.Fatal(deleted, e)
	}
	if _, e = p.f.s.PrepareIssueGet(ctx, workerCaller(), apicontract.IssueGetArgs{ID: id}); e == nil {
		t.Fatal("deleted issue still visible")
	}
}
func issueReadBudgets(t *testing.T, p *peopleFixture) {
	for _, q := range []apicontract.IssueListArgs{{Kind: "other"}, {State: "other"}, {WorkStatus: "other"}, {StaleAfter: "-1m"}, {ActiveStaleAfter: "0s"}} {
		if _, e := p.f.s.PrepareIssueList(t.Context(), workerCaller(), q); e == nil {
			t.Fatal("invalid selector accepted", q)
		}
	}
	small := createTask(t, p, "origin", strings.Repeat("x", 70<<10))
	call, e := p.f.s.PrepareIssueGetMany(t.Context(), workerCaller(), apicontract.IssueGetManyArgs{IDs: []int64{small.ID, small.ID}})
	got, e := execute(t.Context(), call, e)
	if e != nil || got.Issues[0].Issue == nil || got.Issues[1].Error == nil {
		t.Fatal("complete-row budget refusal missing", got, e)
	}
	raw, e := json.Marshal(got)
	if e != nil || len(raw) > 128<<10 {
		t.Fatal("envelope budget exceeded", len(raw), e)
	}
	issueCreateBudgets(t, p, small)
}

func issueCreateBudgets(t *testing.T, p *peopleFixture, small model.Issue) {
	t.Helper()
	for _, extra := range []int{0, 1} {
		projected := small.Clone()
		projected.ID += 1 + int64(extra)
		projected.Title, projected.Body = "t", ""
		projected.Author = workerCaller().Actor.Identity
		raw, err := json.Marshal(projected)
		if err != nil {
			t.Fatal(err)
		}
		body := strings.Repeat("b", model.IssueBytes-len(raw)+extra)
		call, e := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Title: "t", Body: &body, Upstream: "origin"})
		created, e := execute(t.Context(), call, e)
		if (e != nil) != (extra == 1) {
			t.Fatal("complete issue N/N+1", extra, e)
		}
		if e == nil {
			read, err := p.f.s.PrepareIssueGet(t.Context(), workerCaller(), apicontract.IssueGetArgs{ID: created.ID})
			if _, err = execute(t.Context(), read, err); err != nil {
				t.Fatal("successful create is unreadable", err)
			}
		}
	}
}

func assertIssueManyOrder(t *testing.T, p *peopleFixture, ids []int64) {
	t.Helper()
	many, e := p.f.s.PrepareIssueGetMany(t.Context(), workerCaller(), apicontract.IssueGetManyArgs{IDs: ids})
	rows, e := execute(t.Context(), many, e)
	if e != nil {
		t.Fatal(e)
	}
	if len(rows.Issues) != 4 || rows.Issues[0].Error == nil || rows.Issues[1].Issue == nil || rows.Issues[1].Issue.Body != "complete body" || rows.Issues[2].Error == nil || rows.Issues[3].ID != ids[1] {
		t.Fatal("ordered independent issue results", rows)
	}
}

func assertIssueReadDoesNotExtend(t *testing.T, p *peopleFixture, v model.Issue, lock model.IssueLock) {
	t.Helper()
	ctx := t.Context()
	for range 2 {
		read, e := p.f.s.PrepareIssueGet(ctx, workerCaller(), apicontract.IssueGetArgs{ID: v.ID, StaleAfter: "1m"})
		got, e := execute(ctx, read, e)
		if e != nil || got.Revision != v.Revision || got.Lock == nil || !got.Lock.ExpiresAt.Equal(lock.ExpiresAt) {
			t.Fatal("read changed lock/revision", got, e)
		}
	}
}

func TestIssueGetManyAcceptsOneCompleteSavePointIssue(t *testing.T) {
	for _, real := range []bool{false, true} {
		t.Run(fmt.Sprint(real), func(t *testing.T) {
			p := people(t)
			if !real {
				p.f.c.People.Issues = p.f.store.native
				p.f.restart(t)
			}
			checkIssueGetManyCompleteIssue(t, p)
		})
	}
}

func checkIssueGetManyCompleteIssue(t *testing.T, p *peopleFixture) {
	t.Helper()
	sample := createTask(t, p, "origin", "")
	projected := sample.Clone()
	projected.ID++
	raw, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	large := createTask(t, p, "origin", strings.Repeat("b", model.IssueBytes-len(raw)))
	raw, err = json.Marshal(large)
	if err != nil || len(raw) != model.IssueBytes {
		t.Fatal("test did not reach complete bound", len(raw), err)
	}
	read, err := p.f.s.PrepareIssueGetMany(t.Context(), workerCaller(), apicontract.IssueGetManyArgs{IDs: []int64{large.ID}})
	got, err := execute(t.Context(), read, err)
	if err != nil || len(got.Issues) != 1 || got.Issues[0].Issue == nil || got.Issues[0].Issue.Body != large.Body {
		t.Fatal("accepted issue cannot be read alone through many", err)
	}
	read, err = p.f.s.PrepareIssueGetMany(t.Context(), workerCaller(), apicontract.IssueGetManyArgs{IDs: []int64{large.ID, sample.ID}})
	got, err = execute(t.Context(), read, err)
	if err != nil || got.Issues[0].Error == nil || got.Issues[1].Issue == nil {
		t.Fatal("combined budget did not refuse independently", err)
	}
	refusal := publicerr.Classify(got.Issues[0].Error)
	want := fmt.Sprintf("Read issue %d by itself with issue_get or issue_get_many.", large.ID)
	if refusal.Remedy() != want {
		t.Fatal("unhelpful combined remedy", refusal.Remedy())
	}
}

func assertScopedIssueCreate(t *testing.T, p *peopleFixture) {
	t.Helper()
	auto, e := p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Title: "scoped", Body: new("")})
	created, e := execute(t.Context(), auto, e)
	if e != nil || created.Issue.Upstream != "origin" {
		t.Fatal("uplink did not select its sole visible upstream", e)
	}
	if _, e = p.f.s.PrepareIssueCreate(t.Context(), workerCaller(), apicontract.IssueCreateArgs{Upstream: "global", Title: "global", Body: new("")}); e == nil {
		t.Fatal("uplink created global issue")
	}
}
