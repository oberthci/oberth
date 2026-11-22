package store

import (
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

// A node observed again with the instants it was recorded with keeps its
// evidence, also when the observation carries them in another location: the
// native API reads times in local time and the store reads its own back in
// UTC, so each observation after the first (and every one after a restart)
// was refused. An instant that changed is still refused.
func TestNodeEvidenceComparesInstantsNotTheirLocation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	started := time.Unix(2000000000, 0).In(time.FixedZone("UTC", 0))
	running := stateport.Node{NodeID: "node", Path: "build", Attempt: 1, Status: stepmodel.StepRunning, Credential: new(false), StartedAt: &started}
	requireOK(t, putNodes(t, s, evidenceInput{RunID: r.ID, Nodes: []stateport.Node{running}}))
	finished := started.Add(time.Minute)
	done := running
	done.Status, done.Exit, done.FinishedAt = stepmodel.StepPassed, new(0), &finished
	if err := putNodes(t, s, evidenceInput{RunID: r.ID, Nodes: []stateport.Node{done}}); err != nil {
		t.Fatal("an observation with the recorded instants was refused", err)
	}
	nodes, err := s.Nodes(t.Context(), r.ID)
	requireOK(t, err)
	if len(nodes) != 1 || nodes[0].Status != stepmodel.StepPassed || nodes[0].FinishedAt == nil || !nodes[0].FinishedAt.Equal(finished) {
		t.Fatalf("the later observation was not recorded: %#v", nodes)
	}
	moved := started.Add(time.Second)
	changed := done
	changed.StartedAt = &moved
	err = putNodes(t, s, evidenceInput{RunID: r.ID, Nodes: []stateport.Node{changed}})
	if err == nil || publicerr.Classify(err).Code() != publicerr.Conflict {
		t.Fatal("a changed start instant was accepted", err)
	}
}
