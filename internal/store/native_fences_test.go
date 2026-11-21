package store

import (
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestNativeTerminalRegressionAndEvidenceRetentionRefuse(t *testing.T) {
	t.Parallel()
	for _, mode := range []string{"terminal", "exit", "start", "finish"} {
		t.Run(mode, func(t *testing.T) {
			s := testStore(t)
			r, _ := accepted(t, s, "run")
			node := completeNode(s)
			a := stateport.Action{Actor: "operator"}
			requireOK(t, putNodes(t, s, evidenceInput{Action: a, RunID: r.ID, Nodes: []stateport.Node{node}}))
			code := publicerr.Conflict
			switch mode {
			case "terminal":
				node.Status = stepmodel.StepRunning
				code = publicerr.InvalidTransition
			case "exit":
				node.Exit = nil
			case "start":
				node.StartedAt = nil
			case "finish":
				node.FinishedAt = nil
			}
			requireCode(t, putNodes(t, s, evidenceInput{Action: a, RunID: r.ID, Nodes: []stateport.Node{node}}), code)
		})
	}
}

func TestCompletionRequiresCredentialObservation(t *testing.T) {
	t.Parallel()
	s := testStore(t)
	r, _ := accepted(t, s, "run")
	node := completeNode(s)
	node.Credential = nil
	requireOK(t, putNodes(t, s, evidenceInput{RunID: r.ID, Nodes: []stateport.Node{node}}))
	requireCode(t, completeEvidence(t, s, evidenceInput{RunID: r.ID}), publicerr.Conflict)
}
