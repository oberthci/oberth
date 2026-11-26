package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func acceptLogRun(t *testing.T, l serverLogs, input stateport.Acceptance) stateport.Run {
	t.Helper()
	ci := input.Request
	result, err := l.owner.c.store.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: ci.ReceiveID, Repository: ci.Repo, Actor: ci.Actor, Runs: []stateport.Acceptance{input}})
	if err != nil || len(result.Runs) != 1 {
		t.Fatal(result, err)
	}
	return result.Runs[0]
}

func sealLogView(t *testing.T, v admissioncontract.PreparedView) *admissioncontract.PreparedAdmission {
	t.Helper()
	p, err := admissioncontract.Seal(v)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func inPodLogPredecessor(t *testing.T) (serverLogs, stateport.Run) {
	t.Helper()
	l, base := realServerLogReader(t)
	v := base.Admission.Clone()
	v.RunID = "held-log-run"
	v.Plan.Entries[0].HoldEligible, v.Plan.Entries[0].HoldArmed = true, true
	v.SourceBindings = []sourcecontract.Binding{{Template: "build", Artifact: "source", Path: "/work/src", RepositoryID: base.RepositoryID, Descriptor: sourcecontract.Descriptor{Version: 1, Kind: sourcecontract.Main}, Revision: sourcecontract.Revision{Repository: base.Repo, Commit: *base.CommitSHA}}}
	ci := model.CIRequest{ReceiveID: "held-log-run", Repo: base.Repo, Ref: base.Ref, ObjectSHA: base.ObjectSHA, CommitSHA: base.CommitSHA, Trigger: model.TriggerBranch, Assist: true, Actor: actor.Actor{Identity: "operator", AuthMethod: "cert"}}
	r := acceptLogRun(t, l, stateport.Acceptance{Request: ci, Prepared: sealLogView(t, v)})
	b := nativebinding.RunBinding{RunID: r.ID, RepositoryID: r.RepositoryID, Ref: r.Ref, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, Trigger: string(model.TriggerBranch), Identity: r.Admission.Digest, SpecSHA256: r.Admission.SpecDigest, Workflow: nativebinding.Object{Namespace: "ci", Name: "shared", UID: "uid"}}
	if err := l.owner.c.store.BindWorkflow(t.Context(), stateport.Action{Actor: "operator"}, b); err != nil {
		t.Fatal(err)
	}
	return l, r
}

func resumeLogRun(t *testing.T, l serverLogs, old stateport.Run, id string) stateport.Run {
	t.Helper()
	path := old.Admission.Plan.Entries[0].Path
	if err := l.owner.c.store.SaveProgress(t.Context(), old.ID, stateport.Progress{Steps: []stepmodel.RunStep{{Path: path, Name: "build", Status: stepmodel.StepRunning}}}); err != nil {
		t.Fatal(err)
	}
	old, err := l.owner.c.store.Run(t.Context(), old.ID)
	if err != nil {
		t.Fatal(err)
	}
	fix := strings.Repeat("f", 40) // Equal-commit chains still need distinct log markers.
	v := old.Admission.Clone()
	v.RunID, v.ObjectSHA, v.CommitSHA = id, fix, fix
	v.SourceBindings[0].Revision.Commit = fix
	h := &nativebinding.ResumeHold{RunID: id, PodName: "pod", PodUID: "pod-uid", NodeID: "node", Step: path, OriginalExit: 7, HeldSince: time.Unix(100, 0), Digest: strings.Repeat("d", 64), Source: v.SourceBindings[0]}
	plan := nativebinding.ResumePlan{Workflow: &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: "shared", Labels: map[string]string{nativebinding.RunLabel: id}}}, UID: "uid", ResourceVersion: "7", Remaining: []string{path}, InPod: h}
	body, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	ci := model.CIRequest{ReceiveID: id, Repo: old.Repo, Ref: old.Ref, ObjectSHA: fix, CommitSHA: &fix, Trigger: model.TriggerBranch, Assist: true, Actor: actor.Actor{Identity: "operator", AuthMethod: "cert"}}
	return acceptLogRun(t, l, stateport.Acceptance{Request: ci, Prepared: sealLogView(t, v), Resume: &model.RunOrigin{ResumedFrom: old.ID, ResumeUID: old.Origin.NativeUID(), WorkflowName: "shared", Assisted: true, ResumePlan: body}})
}

func captureResumedLog(t *testing.T, l serverLogs, r stateport.Run, body string) {
	t.Helper()
	entry := runlog.ContainerLog{NodeID: "node", Step: r.Admission.Plan.Entries[0].Path, PodUID: "pod-uid", PodName: "pod", Namespace: "ci", Container: "main", Main: true}
	reservation, err := l.owner.c.logs.Reserve(t.Context(), r.ID, entry)
	if err != nil {
		t.Fatal(err)
	}
	if err = reservation.Capture(t.Context(), runlog.CaptureRequest{Source: io.NopCloser(strings.NewReader(body))}); err != nil {
		t.Fatal(err)
	}
}

func TestRunLogsPredecessorUsesSuccessorFailedSpan(t *testing.T) {
	t.Parallel()
	l, old := inPodLogPredecessor(t)
	next := resumeLogRun(t, l, old, "resumed-logs")
	marker := nativebinding.RerunMarker(*next.CommitSHA, next.ID)
	captureResumedLog(t, l, next, "P0 failed\n"+marker+"\nF passed\n")
	q := apicontract.RunLogsArgs{ID: old.ID, Step: "build"}
	result, err := l.Read(t.Context(), old, q)
	if err != nil || result.ID != old.ID || result.Content != "P0 failed\n" {
		t.Fatal("R1 read F", result, err)
	}
	if result.Usage == nil || result.Usage.PhysicalRunID != next.ID || result.Usage.Body.Logical == 0 {
		t.Fatal("failed span lost log usage", result)
	}
	q.Pattern = new("F passed")
	result, err = l.Read(t.Context(), old, q)
	if err != nil || result.Content != "" {
		t.Fatal("filter escaped R1 span", result, err)
	}
}

func TestRunLogsRepeatedInPodResumeSeparatesEqualCommitAttempts(t *testing.T) {
	t.Parallel()
	l, first := inPodLogPredecessor(t)
	second := resumeLogRun(t, l, first, "resume-two")
	third := resumeLogRun(t, l, second, "resume-three")
	marker2 := nativebinding.RerunMarker(*second.CommitSHA, second.ID)
	marker3 := nativebinding.RerunMarker(*third.CommitSHA, third.ID)
	captureResumedLog(t, l, third, "P0 failed\n"+marker2+"\nF failed\n"+marker3+"\nF passed\n")
	for _, test := range []struct {
		run  stateport.Run
		want string
	}{{first, "P0 failed\n"}, {second, "F failed\n"}} {
		result, err := l.Read(t.Context(), test.run, apicontract.RunLogsArgs{ID: test.run.ID, NodeID: "node", Container: "main"})
		if err != nil || result.Content != test.want {
			t.Fatal("attempts mixed", test.run.ID, result, err)
		}
		if result.Usage == nil || result.Usage.PhysicalRunID != third.ID || result.Usage.Body.Logical == 0 {
			t.Fatal("attempt usage lost physical archive", test.run.ID, result)
		}
	}
}
