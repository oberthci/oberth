package service

import (
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

func nativeReplayAttempt(rows ...model.StepResult) model.RunReplayAttempt {
	observation := &model.RunTerminalObservation{Version: 1, WorkflowName: "workflow", Nodes: len(rows), NodeIDs: []string{}}
	for _, row := range rows {
		observation.NodeIDs = append(observation.NodeIDs, row.NodeID)
	}
	return model.RunReplayAttempt{Run: model.Run{Status: model.RunFailed, WorkflowName: "workflow"}, Steps: rows, Observation: observation}
}

func TestNativeReplaySafetyUsesAllNodeTemplatesAndGrantEvidence(t *testing.T) {
	plain := model.StepResult{ReplayEvidenceComplete: true, NodeID: "plain-node", Template: "plain", NodeType: "Pod", Phase: "Succeeded", Stage: "check", Step: "check", Status: model.StepPassed, CredentialEvidenceKnown: true, GrantEvidence: []string{}}
	secret := plain
	secret.NodeID, secret.Template, secret.Credentialed = "recursive-secret-node", "credentialed", true
	secret.GrantEvidence = []string{"secret:approved-path"}
	for _, test := range []struct {
		name    string
		attempt model.RunReplayAttempt
		want    bool
	}{
		{"plain", nativeReplayAttempt(plain), true},
		// Recursion and retries can have a display key that a cut plan labels
		// non-secret. The actual node template and admitted grants decide.
		{"recursive-display-key-collision", nativeReplayAttempt(plain, secret), false},
		{"earlier-credentialed-retry", nativeReplayAttempt(secret, plain), false},
		{"credentialed", nativeReplayAttempt(secret), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, safe := (&API{}).releaseAttemptSafe(test.attempt); safe != test.want {
				t.Fatalf("replay safety = %v, want %v", safe, test.want)
			}
		})
	}
}

func TestNativeReplaySafetyFailsClosedOnMissingOrLegacyEvidence(t *testing.T) {
	row := model.StepResult{ReplayEvidenceComplete: true, NodeID: "node", Template: "plain", NodeType: "Pod", Phase: "Failed", Status: model.StepFailed, CredentialEvidenceKnown: true, GrantEvidence: []string{}}
	for _, mode := range []string{"cut-graph", "stored-row-without-native-facts", "missing-grants", "missing-template", "missing-row", "foreign-node", "duplicate-node", "receipt-without-native-ids", "running", "publication", "foreign-workflow"} {
		t.Run(mode, func(t *testing.T) {
			attempt := nativeReplayAttempt(row)
			switch mode {
			case "cut-graph":
				attempt.Steps[0].ReplayEvidenceComplete = false
			case "stored-row-without-native-facts":
				attempt.Steps[0] = model.StepResult{NodeID: "stored:1", Stage: "build", Step: "check", Status: model.StepPassed}
				attempt.Observation.NodeIDs[0] = "stored:1"
			case "missing-grants":
				attempt.Steps[0].CredentialEvidenceKnown = false
			case "missing-template":
				attempt.Steps[0].Template = ""
			case "missing-row":
				attempt.Steps = nil
			case "foreign-node":
				attempt.Observation.NodeIDs[0] = "other-node"
			case "duplicate-node":
				attempt.Observation.Nodes = 2
				attempt.Observation.NodeIDs = []string{"node", "node"}
				attempt.Steps = append(attempt.Steps, row)
			case "receipt-without-native-ids":
				attempt.Observation.NodeIDs = nil
			case "running":
				attempt.Steps[0].Status = model.StepRunning
			case "publication":
				attempt.PublicationStarted = true
			case "foreign-workflow":
				attempt.Observation.WorkflowName = "other-workflow"
			}
			if _, safe := (&API{}).releaseAttemptSafe(attempt); safe {
				t.Fatal("incomplete or foreign evidence authorized replay")
			}
		})
	}
}

func TestNativeReplaySkippedAndOmittedNodesDidNotRun(t *testing.T) {
	for _, phase := range []string{"Skipped", "Omitted"} {
		row := model.StepResult{ReplayEvidenceComplete: true, NodeID: "skipped", Template: "credentialed", NodeType: "Skipped", Phase: phase, Status: model.StepSkipped}
		if _, safe := (&API{}).releaseAttemptSafe(nativeReplayAttempt(row)); !safe {
			t.Fatalf("native %s node counted as execution", phase)
		}
	}
}

func TestNativeReplayRefusesCutVirtualAndSkippedOnlyGraphs(t *testing.T) {
	for _, kind := range []string{"DAG", "Steps", "StepGroup", "Retry", "TaskGroup", "Skipped"} {
		row := model.StepResult{NodeID: "node", NodeType: kind, Phase: "Skipped", Status: model.StepSkipped}
		if _, safe := (&API{}).releaseAttemptSafe(nativeReplayAttempt(row)); safe {
			t.Fatal("cut or missing admission evidence authorized replay", kind)
		}
	}
	if _, safe := (&API{}).releaseAttemptSafe(nativeReplayAttempt()); safe {
		t.Fatal("empty stored history authorized replay after Workflow intent")
	}
}

func TestNativeReplayRefusesExecutionWitnessWithoutAnExecutableNode(t *testing.T) {
	row := model.StepResult{ReplayEvidenceComplete: true, NodeID: "root", NodeType: "DAG", Status: model.StepFailed}
	attempt := nativeReplayAttempt(row)
	attempt.Observation.Steps = []model.RunObservedStep{{Stage: "publish", Step: "push", Status: model.StepFailed, Started: true}}
	if _, safe := (&API{}).releaseAttemptSafe(attempt); safe {
		t.Fatal("execution witness was hidden behind virtual rows")
	}
}

func TestNativeReplayRefusesNamedFailureWithoutExecutableRows(t *testing.T) {
	row := model.StepResult{ReplayEvidenceComplete: true, NodeID: "root", NodeType: "DAG", Status: model.StepFailed}
	attempt := nativeReplayAttempt(row)
	attempt.Run.FailedStage, attempt.Run.FailedStep = "publish", "push"
	if _, safe := (&API{}).releaseAttemptSafe(attempt); safe {
		t.Fatal("named failed step had no native execution evidence")
	}
}
