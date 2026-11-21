package argojob_test

import (
	"context"
	"encoding/json"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
	core "k8s.io/api/core/v1"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const delta2Parent = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  volumes:
  - name: workspace
    persistentVolumeClaim: {claimName: workspace}
  templates:
  - name: main
    dag:
      tasks:
      - {name: group, template: group}
  - name: group
    retryStrategy: {limit: '1', retryPolicy: OnFailure}
    dag:
      tasks:
      - {name: build, template: build}
  - name: build
    container:
      image: example.test/build:1
      command: [sh, -c]
      args: ['if [ -f /workspace/done ]; then test "$(cat /workspace/result)" = background; else printf original > /workspace/result; (while [ ! -S /oberth-hold/state/hold.sock ]; do :; done; printf background > /workspace/result; touch /workspace/done) >/dev/null 2>&1 & exit 7; fi']
      volumeMounts: [{name: workspace, mountPath: /workspace}]
`
const delta2Dynamic = `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  volumes:
  - name: workspace
    persistentVolumeClaim: {claimName: workspace}
  templates:
  - name: main
    dag:
      tasks:
      - name: direct
        template: build
        arguments: {parameters: [{name: mode, value: ok}]}
      - name: loop
        template: group
        withParam: '["x"]'
        dependencies: [direct]
      - {name: check, template: check, dependencies: [loop]}
  - name: group
    dag:
      tasks:
      - name: build
        template: build
        continueOn: {failed: true}
        arguments: {parameters: [{name: mode, value: fail}]}
  - name: build
    inputs: {parameters: [{name: mode}]}
    container:
      image: example.test/build:1
      command: [sh, -c]
      args: ['if [ "{{inputs.parameters.mode}}" = ok ]; then exit 0; fi; printf original > /workspace/result; (while [ ! -S /oberth-hold/state/hold.sock ]; do :; done; printf background > /workspace/result) >/dev/null 2>&1 & exit 7']
      volumeMounts: [{name: workspace, mountPath: /workspace}]
  - name: check
    container:
      image: example.test/build:1
      command: [sh, -c]
      args: ['test "$(cat /workspace/result)" = background']
      volumeMounts: [{name: workspace, mountPath: /workspace}]
      readinessProbe: {exec: {command: ["true"]}}
`

func delta2Admission(t *testing.T, text string) *admissioncontract.PreparedAdmission {
	t.Helper()
	w := wf.MustUnmarshalWorkflow(text)
	document, err := json.Marshal(w)
	compositionOK(t, err)
	r := requestForProgress()
	pipeline := account(t, identityname.PipelineCI, "")
	blob := strings.Repeat("b", 40)
	req := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: r.Repo, RunID: "run-0000000000000001", Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: model.TriggerBranch, SelectedFile: ".oberth/build.yaml", FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, ObjectSHA: r.ObjectSHA, File: ".oberth/build.yaml", Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: pipeline}}
	e := argojob.New(argojob.Config{Accounts: fake.Accounts{Names: map[string]string{pipeline: "pipeline-uid"}}})
	p, err := argoworkflow.Prepare(t.Context(), req, argoworkflow.Policy{PVCClaims: map[string]bool{"workspace": true}}, e.Hooks(nativebinding.Preparation{Repository: r.Repo, File: req.SelectedFile, Assist: true, HelperImage: "oberth.ci/helper:1"}))
	compositionOK(t, err)
	return p
}

func delta2Finish(t *testing.T, p *admissioncontract.PreparedAdmission, w *wf.Workflow) {
	t.Helper()
	delta2FinishObservation(t, p, project(t, argojob.Projection{Plan: p.View().Plan, Workflow: w}))
}
func delta2FinishObservation(t *testing.T, p *admissioncontract.PreparedAdmission, out argojob.Observation) {
	t.Helper()
	if out.Status != stepmodel.RunPassed {
		t.Fatal("fixture not passed", out)
	}
	t.Logf("sealed plan=%+v final native nodes=%+v excluded=%v", p.View().Plan.Entries, out.Nodes, stateport.ArmedStepMayHaveHeld(p.View().Plan, out.Nodes))
	s := compositionStore(t)
	registerCompositionRepository(t, s, "forge/org/repo")
	r := requestForProgress()
	r.Assist = true
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	id := p.View().RunID
	seq, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: len(out.Nodes)})
	compositionOK(t, err)
	compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: seq, Nodes: out.Nodes}))
	run, err := s.Run(t.Context(), id)
	compositionOK(t, err)
	err = s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: id, Revision: run.Revision, Status: out.Status, NativePhase: &out.Phase, ProjectCI: true})
	compositionOK(t, err)
	run, err = s.Run(t.Context(), id)
	compositionOK(t, err)
	if run.Status != stepmodel.RunPassed || run.Origin.Assisted {
		t.Fatalf("unarmed execution is not ordinary evidence: status=%s assisted=%v", run.Status, run.Origin.Assisted)
	}
}

func delta2Leaf(id, name, boundary string, phase wf.NodePhase) wf.NodeStatus {
	n := nativeNode(id, name, phase, 1)
	n.TemplateName = "build"
	if id == "check" {
		n.TemplateName = "check"
	}
	n.BoundaryID = boundary
	code := "0"
	if phase == wf.NodeFailed {
		code = "7"
	}
	n.Outputs = &wf.Outputs{ExitCode: &code}
	return n
}

func TestDelta2ParentRetryCannotEraseHeldFailure(t *testing.T) {
	t.Parallel()
	p := delta2Admission(t, delta2Parent)
	plan := p.View().Plan
	if len(plan.Entries) != 1 || plan.Entries[0].HoldArmed || plan.Entries[0].HoldEligible || plan.Entries[0].HoldRefusal != "inside a retried group" {
		t.Fatal("parent retry leaf remained eligible", plan)
	}
	leaf := p.View().Workflow.GetTemplateByName("build")
	if leaf.Container.Command[0] == "/oberth-hold/bin/oberth-hold" {
		t.Fatal("parent retry leaf wrapped")
	}
	for _, v := range leaf.Container.Env {
		if (v.Name == "OBERTH_HOLD_ATTEMPT" || v.Name == "OBERTH_HOLD_LIMIT") && v.Value != "0" {
			t.Fatal("leaf cannot hold on first failure", v)
		}
	}
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowSucceeded
	w.Status.Nodes = wf.Nodes{
		"root":   {ID: "root", Name: w.Name, Type: wf.NodeTypeDAG, Phase: wf.NodeSucceeded, TemplateName: "main"},
		"retry":  {ID: "retry", Name: w.Name + ".group", Type: wf.NodeTypeRetry, Phase: wf.NodeSucceeded, Children: []string{"g0", "g1"}},
		"g0":     {ID: "g0", Name: w.Name + ".group(0)", Type: wf.NodeTypeDAG, Phase: wf.NodeFailed, TemplateName: "group", Children: []string{"held"}},
		"g1":     {ID: "g1", Name: w.Name + ".group(1)", Type: wf.NodeTypeDAG, Phase: wf.NodeSucceeded, TemplateName: "group", Children: []string{"passed"}},
		"held":   delta2Leaf("held", ".group(0).build", "g0", wf.NodeFailed),
		"passed": delta2Leaf("passed", ".group(1).build", "g1", wf.NodeSucceeded),
	}
	delta2NoHolds(t, p, w)
	delta2Finish(t, p, w)
}

func TestDelta2ReusedTemplateInsideDynamicGroupIsCovered(t *testing.T) {
	t.Parallel()
	p := delta2Admission(t, delta2Dynamic)
	plan := p.View().Plan
	if len(plan.Entries) != 3 || plan.Entries[0].HoldArmed || plan.Entries[0].HoldEligible || !plan.Entries[1].Dynamic || plan.Entries[1].HoldArmed || plan.Entries[0].HoldRefusal != "its template is also used where assist cannot apply" {
		t.Fatal("fixture plan", plan)
	}
	leaf := p.View().Workflow.GetTemplateByName("build")
	if leaf.Container.Command[0] == "/oberth-hold/bin/oberth-hold" {
		t.Fatal("shared template wrapped")
	}
	w := nativeWorkflow()
	w.Status.Phase = wf.WorkflowSucceeded
	w.Status.Nodes = wf.Nodes{
		"root":   {ID: "root", Name: w.Name, Type: wf.NodeTypeDAG, Phase: wf.NodeSucceeded, TemplateName: "main"},
		"group":  {ID: "group", Name: w.Name + ".loop(0:x)", Type: wf.NodeTypeDAG, Phase: wf.NodeSucceeded, TemplateName: "group", Children: []string{"held"}},
		"check":  delta2Leaf("check", ".check", "root", wf.NodeSucceeded),
		"direct": delta2Leaf("direct", ".direct", "root", wf.NodeSucceeded),
		"held":   delta2Leaf("held", ".loop(0:x).build", "group", wf.NodeFailed),
	}
	delta2NoHolds(t, p, w)
	delta2Finish(t, p, w)
}

func TestDelta2MissingArmedExpansionCannotUseOlderSuccess(t *testing.T) {
	t.Parallel()
	text := delta2Loop
	p := delta2Admission(t, text)
	if len(p.View().Plan.Entries) != 1 || p.View().Plan.Entries[0].HoldArmed || p.View().Plan.Entries[0].HoldEligible || !p.View().Plan.Entries[0].Dynamic || p.View().Plan.Entries[0].HoldRefusal != "inside a loop" {
		t.Fatal("loop remained eligible", p.View().Plan)
	}
	before := nativeWorkflow()
	a := delta2Leaf("a", ".loop(0:a)", "root", wf.NodeSucceeded)
	before.Status.Nodes = wf.Nodes{"a": a}
	observed := project(t, argojob.Projection{Workflow: before, Plan: p.View().Plan})
	// The later b invocation holds/fails between observations; final status has
	// offloaded nodes, so neither that invocation nor its final result is available.
	final := nativeWorkflow()
	final.Status.Phase = wf.WorkflowSucceeded
	final.Status.Nodes = nil
	final.Status.OffloadNodeStatusVersion = "unavailable"
	out := project(t, argojob.Projection{Workflow: final, Plan: p.View().Plan, Previous: observed.Steps, PreviousNodes: observed.Nodes})
	if out.Complete {
		t.Fatal("fixture must lack final nodes")
	}
	delta2NoHolds(t, p, before)
	delta2FinishObservation(t, p, out)
}

const delta2Loop = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: main
  volumes:
  - name: workspace
    persistentVolumeClaim: {claimName: workspace}
  templates:
  - name: main
    dag:
      tasks:
      - name: loop
        template: build
        withParam: '["a","b"]'
        continueOn: {failed: true}
        arguments: {parameters: [{name: mode, value: "{{item}}"}]}
  - name: build
    inputs: {parameters: [{name: mode}]}
    container:
      image: example.test/build:1
      command: [sh, -c]
      args: ['if [ "{{inputs.parameters.mode}}" = a ]; then exit 0; fi; printf original > /workspace/result; (while [ ! -S /oberth-hold/state/hold.sock ]; do :; done; printf background > /workspace/result) >/dev/null 2>&1 & exit 7']
      volumeMounts: [{name: workspace, mountPath: /workspace}]
`

func delta2NoHolds(t *testing.T, p *admissioncontract.PreparedAdmission, w *wf.Workflow) {
	t.Helper()
	native := &fake.Workflows{}
	owner := memoryAdmission(t, p)
	create := argojob.New(argojob.Config{Workflows: native, Runs: owner})
	b, err := create.Create(t.Context(), p)
	compositionOK(t, err)
	native.Object.Status.Phase = wf.WorkflowRunning
	native.Object.Status.Nodes = wf.Nodes{}
	var pods []*core.Pod
	for id, n := range w.Status.Nodes {
		n.Name = strings.Replace(n.Name, w.Name, b.Workflow.Name, 1)
		if n.Type == wf.NodeTypePod {
			n.Phase = wf.NodeRunning
			pod := nativePod(native.Object, n, 0)
			pod.Status.Phase = core.PodRunning
			pod.Status.ContainerStatuses[0].State = core.ContainerState{Running: &core.ContainerStateRunning{}}
			pod.Status.ContainerStatuses[0].Ready = true
			pods = append(pods, pod)
		}
		native.Object.Status.Nodes[id] = n
	}
	calls := 0
	remote := &fake.Pods{ExecFunc: func(context.Context, argojob.ExecRequest) error {
		calls++
		return errors.New("unexpected helper dispatch")
	}}
	e := argojob.New(argojob.Config{Workflows: native, Pods: remote, Cache: &fake.Cache{WorkflowObject: native.Object, PodObjects: pods}})
	holds, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	if len(holds) != 0 || calls != 0 {
		t.Fatal("unarmed invocation exposed a hold", holds, calls)
	}
}

func TestDelta2ActualNativeResultsStayOrdinary(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	for name, document := range map[string]string{"parent": delta2Parent, "dynamic": delta2Dynamic} {
		p := delta2Admission(t, document)
		raw, err := json.Marshal(p.View().Workflow)
		compositionOK(t, err)
		compositionOK(t, os.WriteFile(filepath.Join(directory, name+"-input.json"), raw, 0600))
	}
	delta2Controller(t, directory)
	for name, document := range map[string]string{"parent": delta2Parent, "dynamic": delta2Dynamic} {
		t.Run(name, func(t *testing.T) {
			p := delta2Admission(t, document)
			var w wf.Workflow
			readNativeJSON(t, filepath.Join(directory, name+"-native.json"), &w)
			delta2NoHolds(t, p, &w)
			delta2Finish(t, p, &w)
		})
	}
}
func delta2Controller(t *testing.T, directory string) {
	t.Helper()
	command := holdControllerFixture.Command(t.Context(), t, "-test.count=1", "-test.timeout=120s", "-test.run=^TestDelta2NativeHeldEvidenceShapes$")
	command.Env = append(os.Environ(), "OBERTH_HOLD_CONTROLLER_CASES="+directory)
	raw, err := controllertest.Output(command)
	if err != nil {
		t.Fatalf("native controller: %v\n%s", err, raw)
	}
}
