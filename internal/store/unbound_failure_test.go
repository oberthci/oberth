package store

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFailUnboundPersistsFullDetailAndRetriesAfterReopen(t *testing.T) {
	t.Parallel()
	for _, reason := range []string{stateport.ReasonCreateRefused, stateport.ReasonRetentionRefused, stateport.ReasonRestartAbsent, stateport.ReasonWorkflowOwner} {
		t.Run(reason, func(t *testing.T) { checkUnboundFailureReopen(t, reason) })
	}
}

func checkUnboundFailureReopen(t *testing.T, reason string) {
	t.Helper()
	s := testStore(t)
	r, _ := accepted(t, s, "unbound")
	input := stateport.UnboundFailure{RunID: r.ID, Reason: reason, Detail: strings.Repeat("full refusal detail\n", 400)}
	before := independentHead(t, s.readers)
	action := stateport.Action{Actor: "operator", Events: []stateport.EventWrite{atomicEvent(s, "unbound-failure")}}
	requireOK(t, s.FailUnbound(t.Context(), action, input))
	after := independentHead(t, s.readers)
	if after.ID != before.ID+1 {
		t.Fatal("failure not audited")
	}
	path, options := s.path, s.options
	requireOK(t, s.Close())
	reopened, err := Open(t.Context(), path, options)
	requireOK(t, err)
	defer func() { requireOK(t, reopened.Close()) }()
	got, err := reopened.Run(t.Context(), r.ID)
	requireOK(t, err)
	if got.Status != stepmodel.RunFailed || got.Revision != r.Revision+1 || got.DisplayReason != reason || got.Detail != input.Detail || got.FinishedAt == nil || got.Origin.WorkflowUID != "" {
		t.Fatal("unbound failure lost its verdict, diagnostics", got)
	}
	requireOK(t, reopened.FailUnbound(t.Context(), action, input))
	if independentHead(t, reopened.readers) != after {
		t.Fatal("retry repeated audit")
	}
	page, err := reopened.Events(t.Context(), stateport.EventFilter{Limit: 200})
	requireOK(t, err)
	if len(page.Events) != 1 {
		t.Fatal("retry repeated events", page)
	}
	input.Detail = "changed failure"
	requireCode(t, reopened.FailUnbound(t.Context(), action, input), publicerr.Conflict)
	requireCode(t, reopened.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, runBinding(r)), publicerr.Conflict)
}

func TestFailUnboundAtomicWithAuditAndEvents(t *testing.T) {
	t.Parallel()
	checkAtomicFacts(t, []atomicFact{{"run.fail.unbound", func(t *testing.T, s *Store) func(stateport.Action) error {
		r, _ := accepted(t, s, "unbound")
		return func(action stateport.Action) error {
			return s.FailUnbound(t.Context(), action, stateport.UnboundFailure{RunID: r.ID, Reason: stateport.ReasonCreateRefused, Detail: "full diagnostic"})
		}
	}}})
}

func TestFailUnboundCancelledContextWritesNothing(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "unbound")
	before := databaseContents(t, s.readers)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := s.FailUnbound(ctx, stateport.Action{Actor: "operator"}, stateport.UnboundFailure{RunID: r.ID, Reason: stateport.ReasonCreateRefused})
	if err == nil {
		t.Fatal("cancelled failure succeeded")
	}
	if before != databaseContents(t, s.readers) {
		t.Fatal("cancelled failure wrote state")
	}
}

func TestFailUnboundRefusesBoundRejectedAndTerminalRuns(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	rejected := rejectedFixture(t, s)
	r, err := acceptForTest(t.Context(), s, rejected)
	requireOK(t, err)
	input := stateport.UnboundFailure{RunID: r.ID, Reason: stateport.ReasonCreateRefused}
	requireCode(t, s.FailUnbound(t.Context(), stateport.Action{Actor: "operator"}, input), publicerr.InvalidTransition)
	bound, _ := accepted(t, s, "bound")
	requireOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, runBinding(bound)))
	input.RunID = bound.ID
	requireCode(t, s.FailUnbound(t.Context(), stateport.Action{Actor: "operator"}, input), publicerr.Conflict)
	finished, _ := accepted(t, s, "finished")
	requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: finished.ID, Revision: finished.Revision, Status: stepmodel.RunPassed}))
	input.RunID = finished.ID
	requireCode(t, s.FailUnbound(t.Context(), stateport.Action{Actor: "operator"}, input), publicerr.Conflict)
}

func TestFailUnboundRequiresLiteralCatalogueReason(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "unbound")
	for _, reason := range []string{"", "caller diagnostic", strings.Repeat("x", 40), strings.Repeat("x", 41)} {
		before := databaseContents(t, s.readers)
		err := s.FailUnbound(t.Context(), stateport.Action{Actor: "operator"}, stateport.UnboundFailure{RunID: r.ID, Reason: reason})
		requireCode(t, err, publicerr.InvalidInput)
		if before != databaseContents(t, s.readers) {
			t.Fatal("invalid reason wrote facts")
		}
	}
}
