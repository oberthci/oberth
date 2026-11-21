package contracttest

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stateport"
)

type issueLifecyclePort interface {
	ScopedIssue(context.Context, stateport.IssueScope, int64) (model.Issue, error)
	CreateIssue(context.Context, stateport.Action, model.Issue) (model.Issue, error)
	UpdateIssue(context.Context, stateport.Action, model.IssuePatch) (model.Issue, error)
	CloseIssue(context.Context, stateport.Action, stateport.IssueClose) error
	ReopenIssue(context.Context, stateport.Action, stateport.IssueClose) error
	ChangeIssueLock(context.Context, stateport.Action, stateport.IssueLockRequest) error
}

// ExportedIssueLifecycle exercises the same maximal export against both owner stores.
func ExportedIssueLifecycle(t *testing.T, p issueLifecyclePort, id int64) {
	t.Helper()
	a := stateport.Action{Actor: "operator", Caller: actor.Actor{Identity: "operator", Admin: true}}
	before := readLifecycleIssue(t, p, id)
	raw, _ := json.Marshal(before)
	if len(raw) > model.IssueBytes-4096 || len(raw) < model.IssueBytes-4096-5 {
		t.Fatal("maximal export has no lifecycle reserve", len(raw))
	}
	mustRun(t, p.CloseIssue(t.Context(), a, stateport.IssueClose{ID: id, Revision: before.Revision}))
	closed := readLifecycleIssue(t, p, id)
	if closed.State != model.IssueClosed {
		t.Fatal("close lost")
	}
	mustRun(t, p.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: id, Owner: a.Actor}))
	locked := readLifecycleIssue(t, p, id)
	if locked.Lock == nil {
		t.Fatal("lock lost")
	}
	mustRun(t, p.ReopenIssue(t.Context(), a, stateport.IssueClose{ID: id, Revision: locked.Revision}))
	after := readLifecycleIssue(t, p, id)
	if after.State != model.IssueOpen || !reflect.DeepEqual(before.History, after.History) || before.Author != after.Author || before.Closer != after.Closer || !reflect.DeepEqual(before.SourceClosedAt, after.SourceClosedAt) {
		t.Fatal("lifecycle changed source evidence")
	}
	checkFullIssueRefusals(t, p, a)
}

func readLifecycleIssue(t *testing.T, p issueLifecyclePort, id int64) model.Issue {
	t.Helper()
	v, err := p.ScopedIssue(t.Context(), stateport.IssueScope{Global: true}, id)
	mustRun(t, err)
	raw, err := json.Marshal(v)
	if err != nil || len(raw) > model.IssueBytes {
		t.Fatal("successful operation left an unreadable issue", len(raw), err)
	}
	return v
}

func checkFullIssueRefusals(t *testing.T, p issueLifecyclePort, a stateport.Action) {
	t.Helper()
	v, err := p.CreateIssue(t.Context(), a, model.Issue{Kind: model.IssueManual, Title: "full"})
	mustRun(t, err)
	v.Revision++
	raw, _ := json.Marshal(v)
	_, err = p.UpdateIssue(t.Context(), a, model.IssuePatch{ID: v.ID, Body: new(strings.Repeat("x", model.IssueBytes-len(raw)))})
	mustRun(t, err)
	before := readLifecycleIssue(t, p, v.ID)
	for _, op := range []func() error{
		func() error {
			return p.CloseIssue(t.Context(), a, stateport.IssueClose{ID: v.ID, Revision: before.Revision})
		},
		func() error {
			return p.ChangeIssueLock(t.Context(), a, stateport.IssueLockRequest{ID: v.ID, Owner: a.Actor})
		},
	} {
		if err := op(); err == nil {
			t.Fatal("metadata growth past the complete budget succeeded")
		}
		if !reflect.DeepEqual(before, readLifecycleIssue(t, p, v.ID)) {
			t.Fatal("refused lifecycle write changed the issue")
		}
	}
}
