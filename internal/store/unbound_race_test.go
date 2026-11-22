package store

import (
	"fmt"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestFailUnboundAndBindingHaveOneLegalWinner(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	other, err := Open(t.Context(), s.path, s.options)
	requireOK(t, err)
	defer func() { requireOK(t, other.Close()) }()
	for i := range 8 {
		t.Run(fmt.Sprint(i), func(t *testing.T) { checkUnboundBindingRace(t, s, other) })
	}
}

func checkUnboundBindingRace(t *testing.T, s, other *Store) {
	t.Helper()
	r, _ := accepted(t, s, "race-"+t.Name())
	binding := runBinding(r)
	binding.Workflow.UID = r.ID
	binding.Workflow.Name = "workflow-" + r.ID
	start := make(chan struct{})
	bound := make(chan error, 1)
	failed := make(chan error, 1)
	input := stateport.UnboundFailure{RunID: r.ID, Reason: stateport.ReasonCreateRefused, Detail: "creation refused"}
	go func() { <-start; bound <- s.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, binding) }()
	go func() { <-start; failed <- other.FailUnbound(t.Context(), stateport.Action{Actor: "operator"}, input) }()
	close(start)
	bindErr, failErr := <-bound, <-failed
	got, err := s.Run(t.Context(), r.ID)
	requireOK(t, err)
	if bindErr == nil {
		requireCode(t, failErr, publicerr.Conflict)
		if got.Status != stepmodel.RunQueued || got.Origin.WorkflowUID != binding.Workflow.UID {
			t.Fatal("bound winner was failed", got)
		}
	} else {
		requireOK(t, failErr)
		requireCode(t, bindErr, publicerr.Conflict)
		if got.Status != stepmodel.RunFailed || got.Origin.WorkflowUID != "" {
			t.Fatal("failed winner was bound", got)
		}
	}
}
