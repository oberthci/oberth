package store

import (
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

func TestNativeObservationsKeepPreparedRunRevision(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"begin", "nodes", "complete"} {
		t.Run(kind, func(t *testing.T) {
			for _, operation := range []string{"finish", "cancel"} {
				t.Run(operation, func(t *testing.T) { checkProgressRevision(t, kind, operation) })
			}
		})
	}
}

func checkProgressRevision(t *testing.T, kind, operation string) {
	t.Helper()
	s := testStore(t)
	r, _ := accepted(t, s, "revision")
	observe := nativeObservation(t, s, r.ID, kind)
	prepared, err := s.RunState(t.Context(), r.ID)
	requireOK(t, err)
	ch := observeChanges(t, s)
	requireOK(t, observe())
	select {
	case <-ch:
	default:
		t.Fatal("native observation did not wake readers")
	}
	after, err := s.RunState(t.Context(), r.ID)
	requireOK(t, err)
	if after.Revision != prepared.Revision {
		t.Fatal("native progress changed the run-control revision", prepared.Revision, after.Revision)
	}
	if operation == "finish" {
		requireOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: prepared.Revision, Status: stepmodel.RunFailed}))
	} else {
		stop, err := s.Cancel(t.Context(), stateport.Action{Actor: "operator"}, stateport.RunCancellation{ID: r.ID, Revision: prepared.Revision})
		requireOK(t, err)
		if stop.Value != "Stop" {
			t.Fatal("prepared cancellation lost its mode", stop)
		}
	}
	final, err := s.RunState(t.Context(), r.ID)
	requireOK(t, err)
	if final.Revision != prepared.Revision+1 {
		t.Fatal("run-control mutation did not advance revision", final)
	}
}

func nativeObservation(t *testing.T, s *Store, id, kind string) func() error {
	t.Helper()
	var sequence int64
	if kind == "nodes" || kind == "complete" {
		var err error
		sequence, err = s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
		requireOK(t, err)
	}
	if kind == "complete" {
		requireOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: sequence, Nodes: []stateport.Node{completeNode(s)}}))
	}
	return func() error {
		switch kind {
		case "begin":
			_, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
			return err
		case "nodes":
			return s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: sequence, Nodes: []stateport.Node{completeNode(s)}})
		case "complete":
			return s.CompleteEvidence(t.Context(), stateport.Action{}, stateport.EvidenceCompletion{RunID: id, Sequence: sequence})
		default:
			return nil
		}
	}
}

func TestFinishRacesWithNativeObservation(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"begin", "nodes", "complete"} {
		t.Run(kind, func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "race")
			observe := nativeObservation(t, s, r.ID, kind)
			prepared, err := s.RunState(t.Context(), r.ID)
			requireOK(t, err)
			start := make(chan struct{})
			done := make(chan error, 2)
			go func() { <-start; done <- observe() }()
			go func() {
				<-start
				done <- s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: prepared.Revision, Status: stepmodel.RunFailed})
			}()
			close(start)
			for range 2 {
				requireOK(t, <-done)
			}
			final, err := s.RunState(t.Context(), r.ID)
			requireOK(t, err)
			if final.Status != stepmodel.RunFailed || final.Revision != prepared.Revision+1 {
				t.Fatal("native progress defeated terminal finish", final)
			}
		})
	}
}
