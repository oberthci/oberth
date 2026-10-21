package model

import (
	"strings"
	"testing"
	"time"
)

func TestTimingProjectionRejectsUnboundedOrRawFields(t *testing.T) {
	now := time.Now().UTC()
	workflow := WorkflowTiming{RunID: strings.Repeat("a", 32), Namespace: "pipelines", Name: "workflow", UID: "11111111-1111-4111-8111-111111111111", SpecIdentity: strings.Repeat("b", 64), CreatedAt: now, Tasks: []TimingTask{{ID: "task", Template: "leaf", Readiness: "all"}}}
	if workflow.Validate() != nil {
		t.Fatal("valid workflow rejected")
	}
	for _, mode := range []string{"identity", "raw-expression", "bound", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			changed := workflow
			changed.Tasks = append([]TimingTask{}, workflow.Tasks...)
			switch mode {
			case "identity":
				changed.SpecIdentity = strings.Repeat("z", 64)
			case "raw-expression":
				changed.Tasks[0].Readiness = "secret-canary"
			case "bound":
				changed.Tasks = make([]TimingTask, MaxTimingTasks+1)
			case "duplicate":
				changed.Tasks = append(changed.Tasks, changed.Tasks[0])
			}
			if changed.Validate() == nil {
				t.Fatal("invalid projection accepted")
			}
		})
	}
	step := StepTiming{Version: 1, RunID: workflow.RunID, Namespace: workflow.Namespace, WorkflowName: workflow.Name, WorkflowUID: workflow.UID, NodeID: "node", Burn: "test", Step: "unit", Unavailable: "snapshot-incomplete", ObservedAt: now, ScriptTiming: "mixed", ImagePullTiming: "not-observed"}
	if step.Validate() != nil {
		t.Fatal("valid unavailable rejected")
	}
	step.ImagePullTiming = "inferred-pending"
	if step.Validate() == nil {
		t.Fatal("inferred image pull accepted")
	}
	step.ImagePullTiming = "not-observed"
	step.Unavailable = "secret-canary"
	if step.Validate() == nil {
		t.Fatal("raw failure persisted")
	}
}
