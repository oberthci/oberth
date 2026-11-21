package contracttest

import (
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"testing"
)

type inspectionEvidenceCase struct {
	name                                 string
	status                               stepmodel.RunStatus
	inspect, release                     bool
	armed, unarmedRestart                bool
	native                               stepmodel.StepStatus
	neverRunSkip, podRestarted, excluded bool
	podName                              string
}

func checkInspectionEvidence(t *testing.T, f RunFixture) {
	for _, test := range []inspectionEvidenceCase{
		{name: "armed_tolerated_failure", excluded: true, status: stepmodel.RunPassed, armed: true, native: stepmodel.StepFailed},
		{name: "armed_error", excluded: true, status: stepmodel.RunPassed, armed: true, native: stepmodel.StepUnknown},
		{name: "armed_missing", excluded: true, status: stepmodel.RunPassed, armed: true},
		{name: "armed_restarted_success", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepPassed, podRestarted: true, excluded: true},
		{name: "unarmed_restarted_success", status: stepmodel.RunPassed, native: stepmodel.StepPassed, podRestarted: true},
		{name: "armed_success_with_unarmed_restart", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepPassed, unarmedRestart: true, excluded: false},
		{name: "armed_success", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepPassed},
		{name: "armed_never_run_skip", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepSkipped, neverRunSkip: true},
		{name: "armed_unproven_skipped", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepSkipped, excluded: true},
		{name: "armed_skipped_with_pod", status: stepmodel.RunPassed, armed: true, native: stepmodel.StepSkipped, neverRunSkip: true, podName: "executed-pod", excluded: true},
		{name: "inspected_never_run_skip", status: stepmodel.RunPassed, inspect: true, armed: true, native: stepmodel.StepSkipped, neverRunSkip: true, excluded: true},
		{name: "unarmed_tolerated_failure", status: stepmodel.RunPassed, native: stepmodel.StepFailed},
		{name: "inspected_all_success", excluded: true, status: stepmodel.RunPassed, inspect: true, armed: true, native: stepmodel.StepPassed},
		{name: "requested_passed", status: stepmodel.RunPassed},
		{name: "requested_failed", status: stepmodel.RunFailed},
		{name: "release_without_exec", status: stepmodel.RunPassed, release: true},
		{name: "inspected_native_pass", excluded: true, status: stepmodel.RunPassed, inspect: true},
		{name: "inspected_native_failure", excluded: true, status: stepmodel.RunFailed, inspect: true},
	} {
		t.Run(test.name, func(t *testing.T) { inspectionEvidence(t, f, test) })
	}
}
func inspectionEvidence(t *testing.T, f RunFixture, test inspectionEvidenceCase) {
	t.Helper()
	p := moving(t, f)
	b := batchFor(t, f, RunCase{ID: test.name})
	b.Runs[0].Request.Assist = true
	if test.armed {
		b.Runs[0].Prepared = f.Admit(t, test.name, b.Runs[0].Request)
	}
	if test.unarmedRestart {
		view := b.Runs[0].Prepared.View()
		view.Plan.Entries = append(view.Plan.Entries, admissioncontract.PlanEntry{Path: "other", Name: "other", HoldEligible: true})
		prepared, err := admissioncontract.Seal(view)
		mustRun(t, err)
		b.Runs[0].Prepared = prepared
	}
	accepted, err := p.AcceptBatch(t.Context(), b)
	mustRun(t, err)
	r := accepted.Runs[0]
	a := stateport.Action{Actor: r.Origin.Actor}
	if test.inspect || test.release {
		q := assistIntent(r)
		if test.release {
			q.Kind, q.Argv, q.TimeoutS = "abandon", nil, 0
		}
		mustRun(t, p.Assist(t.Context(), a, q))
	}
	r, err = p.Run(t.Context(), r.ID)
	mustRun(t, err)
	if !r.Origin.AssistRequested || r.Origin.Assisted != test.inspect {
		t.Fatal("request or inspection mark changed", r.Origin)
	}
	if test.native != "" {
		f.Nodes(t, r.ID, inspectionNodes(test))
	}
	excluded := test.excluded
	r = finishInspectionEvidence(t, p, r, test)
	if test.status == stepmodel.RunPassed {
		inspectionPublication(t, p, r, excluded)
	}
}
func finishInspectionEvidence(t *testing.T, p MovingPort, r stateport.Run, test inspectionEvidenceCase) stateport.Run {
	t.Helper()
	a := stateport.Action{Actor: r.Origin.Actor}
	var err error
	excluded := test.excluded
	q := stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: test.status, ProjectCI: true}
	if test.status == stepmodel.RunPassed {
		phase := stepmodel.NativeSucceeded
		q.NativePhase = &phase
	}
	err = p.Finish(t.Context(), a, q)
	if excluded {
		if err == nil {
			t.Fatal("excluded run projected CI", test.name)
		}
		q.ProjectCI = false
		mustRun(t, p.Finish(t.Context(), a, q))
	} else {
		mustRun(t, err)
	}
	r, err = p.Run(t.Context(), r.ID)
	mustRun(t, err)
	if r.Status != test.status || r.Origin.Assisted != excluded {
		t.Fatal("inspection changed native result", r)
	}
	return r
}

func inspectionPublication(t *testing.T, p MovingPort, r stateport.Run, inspected bool) {
	t.Helper()
	publication := movingPublication(r)
	publication.ID = "publication-" + r.ID
	err := p.PreparePublication(t.Context(), stateport.Action{Actor: r.Origin.Actor}, stateport.PublicationIntent{Publication: publication})
	if inspected {
		if err == nil {
			t.Fatal("audited inspection became publication evidence")
		}
	} else {
		mustRun(t, err)
	}
}

func inspectionNodes(test inspectionEvidenceCase) []stateport.Node {
	node := stateport.Node{NodeID: "native", Path: "build", Attempt: 1, Status: test.native, NeverRunSkip: test.neverRunSkip, PodRestarted: test.podRestarted, PodName: test.podName, Exit: new(0)}
	if test.native == stepmodel.StepSkipped {
		node.Exit = nil
	}
	nodes := []stateport.Node{node}
	if test.unarmedRestart {
		nodes = append(nodes, stateport.Node{NodeID: "unarmed", Path: "other", Attempt: 1, Status: stepmodel.StepPassed, PodRestarted: true, Exit: new(0)})
	}
	return nodes
}
