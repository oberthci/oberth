package argojob_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
	core "k8s.io/api/core/v1"
)

func TestConditionalSkipThroughNativeFinish(t *testing.T) {
	t.Parallel()
	for _, tc := range []conditionalEvidenceCase{
		{name: "when-skipped", capture: "when-skipped"},
		{name: "omitted", capture: "omitted", excluded: true},
		{name: "failed", capture: "failed", excluded: true},
		{name: "errored", capture: "failed", change: "error", excluded: true},
		{name: "skipped-type-omitted-phase", capture: "when-skipped", change: "omitted-phase", excluded: true},
		{name: "skipped-type-error-phase", capture: "when-skipped", change: "skipped-error", excluded: true},
		// Empty expansions cannot be armed, but their captured Skipped/Skipped node
		// is forced onto this static armed path: it never ran and remains ordinary.
		{name: "empty-expansion-on-armed-path", capture: "empty-expansion"},
		{name: "missing", capture: "when-skipped", change: "missing", excluded: true},
		{name: "unreadable", capture: "when-skipped", unreadable: true, excluded: true},
		{name: "pod-node-skipped", capture: "when-skipped", change: "pod-type", excluded: true},
		{name: "skipped-pod", capture: "when-skipped", change: "pod", excluded: true},
		{name: "ambiguous-pods", capture: "when-skipped", change: "ambiguous-pods", excluded: true},
		{name: "prior-pod", capture: "when-skipped", change: "prior-pod", excluded: true},
		{name: "skipped-and-failed", capture: "when-skipped", change: "other-failed", excluded: true},
		{name: "skipped-and-inspected", capture: "when-skipped", inspect: true, excluded: true},
	} {
		t.Run(tc.name, func(t *testing.T) { conditionalFinish(t, tc) })
	}
}

type conditionalEvidenceCase struct {
	name, capture, change         string
	inspect, excluded, unreadable bool
	unarmed, restarted            bool
}

func conditionalFinish(t *testing.T, tc conditionalEvidenceCase) {
	p := conditionalAdmission(t, tc)
	s := compositionStore(t)
	registerCompositionRepository(t, s, p.View().Repository)
	request := requestForProgress()
	request.Assist = !tc.unarmed
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: request.ReceiveID, Repository: request.Repo, Actor: request.Actor, Runs: []stateport.Acceptance{{Request: request, Prepared: p}}})
	compositionOK(t, err)
	native := &nativefake.Workflows{}
	c := argojob.Config{Workflows: native, Runs: s, Progress: s, Now: compositionClock}
	_, err = argojob.New(c).Create(t.Context(), p)
	compositionOK(t, err)
	native.Object.Status = conditionalStatus(t, tc.capture, native.Object.Name)
	pods := conditionalChange(t, tc.change, native.Object)
	if tc.change == "prior-pod" || tc.change == "prior-restart" {
		seedConditionalPod(t, s, native.Object, tc.change)
	}
	if tc.inspect {
		run, err := s.Run(t.Context(), p.View().RunID)
		compositionOK(t, err)
		compositionOK(t, s.Assist(t.Context(), stateport.Action{Actor: "fixture", Reason: "inspect"}, stateport.HoldAction{RunID: run.ID, Step: "build", NodeID: "inspection-node", PodUID: "inspection-pod", Kind: "exec", Revision: run.Revision, Argv: []string{"true"}, TimeoutS: 60}))
	}
	var runs service.RunStore = s
	if tc.unreadable {
		runs = unreadableConditionalEvidence{s}
	}
	svc := conditionalService(t, s, runs)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	if tc.unarmed {
		c.Progress = cancelAfterRestartEvidence{Store: s, cancel: cancel}
	}
	calls := 0
	var finishErr error
	c.Cache = &nativefake.Cache{WorkflowObject: native.Object, PodObjects: pods}
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		defer func() {
			if !tc.unarmed {
				cancel()
			}
		}()
		calls++
		finishErr = svc.Finished(ctx, service.FinishedRun{RunID: id, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Detail: o.Detail, Failure: o.Failure})
		return finishErr
	}}
	if err := argojob.New(c).Run(ctx); !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("native finish was not acknowledged once", calls)
	}
	if tc.unreadable {
		if finishErr == nil {
			t.Fatal("finish accepted unreadable native evidence")
		}
		assertUnreadableConditionalFinish(t, s, p.View().RunID)
		return
	}
	compositionOK(t, finishErr)
	assertConditionalNodes(t, s, p.View().RunID, tc)
	assertConditionalFinish(t, s, p.View().RunID, tc.excluded)
}

type unreadableConditionalEvidence struct{ *store.Store }

func (unreadableConditionalEvidence) Nodes(context.Context, string) ([]stateport.Node, error) {
	return nil, errors.New("native evidence unreadable")
}

func assertUnreadableConditionalFinish(t *testing.T, s *store.Store, id string) {
	t.Helper()
	r, err := s.Run(t.Context(), id)
	compositionOK(t, err)
	_, publication := s.Publication(t.Context(), "publication:"+id)
	if r.FinishedAt != nil || s.ProjectCI(t.Context(), stateport.Action{}, id) == nil || publication == nil {
		t.Fatal("unreadable evidence finished or counted as CI")
	}
}

func assertConditionalNodes(t *testing.T, s *store.Store, id string, tc conditionalEvidenceCase) {
	t.Helper()
	nodes, err := s.Nodes(t.Context(), id)
	compositionOK(t, err)
	if strings.HasPrefix(tc.capture, "restart-") {
		if len(nodes) != 1 || nodes[0].PodRestarted != tc.restarted {
			t.Fatal("durable native restart fact differs", nodes, "want", tc.restarted)
		}
		return
	}
	byPath := map[string]stateport.Node{}
	for _, node := range nodes {
		byPath[node.Path] = node
	}
	build, found := byPath["build"]
	if tc.change != "missing" && !found || tc.change == "other-failed" && byPath["other"].Status != stepmodel.StepFailed {
		t.Fatal("fixture lost required native results", nodes)
	}
	if !tc.excluded {
		assertNeverRunSkipNode(t, build)
	}
}

func seedConditionalPod(t *testing.T, s *store.Store, w *wf.Workflow, change string) {
	t.Helper()
	id := w.Annotations["oberth.ci/run-id"]
	if id == "" {
		t.Fatal("fixture workflow has no run binding")
	}
	for _, n := range w.Status.Nodes {
		if n.TemplateName != "build" {
			continue
		}
		podName := "executed-pod"
		if change == "prior-restart" {
			podName = ""
		}
		seq, err := s.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: id, Nodes: 1})
		compositionOK(t, err)
		compositionOK(t, s.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: id, Sequence: seq, Nodes: []stateport.Node{{NodeID: n.ID, Path: strings.TrimPrefix(strings.TrimPrefix(n.Name, w.Name), "."), Template: n.TemplateName, Boundary: n.BoundaryID, PodName: podName, PodRestarted: change == "prior-restart", Attempt: 1, Status: stepmodel.StepRunning, Credential: new(false)}}}))
		return
	}
	t.Fatal("fixture has no armed node")
}

func assertConditionalFinish(t *testing.T, s *store.Store, id string, excluded bool) {
	t.Helper()
	r, err := s.Run(t.Context(), id)
	compositionOK(t, err)
	if r.Origin.Assisted != excluded {
		t.Fatal("wrong evidence eligibility", r.Origin)
	}
	ci := s.ProjectCI(t.Context(), stateport.Action{}, id)
	_, publication := s.Publication(t.Context(), "publication:"+id)
	if excluded && (ci == nil || publication == nil) || !excluded && (ci != nil || publication != nil || r.Status != stepmodel.RunPassed) {
		t.Fatal("CI or publication eligibility differs", ci, publication, r.Status)
	}
	events, err := s.Events(t.Context(), stateport.EventFilter{Limit: 10})
	compositionOK(t, err)
	if len(events.Events) != 1 || events.Events[0].Kind != "run.finished" {
		t.Fatal("finish changed event kinds or count", events)
	}
}

// These statuses were captured from the pinned controller. Only workflow/node
// identity is rebound; native type, phase and skip/omission causes are retained.
func conditionalStatus(t *testing.T, name, workflowName string) wf.WorkflowStatus {
	t.Helper()
	raw, err := os.ReadFile("testdata/armed-evidence-" + name + ".json")
	compositionOK(t, err)
	var status wf.WorkflowStatus
	compositionOK(t, json.Unmarshal(raw, &status))
	oldName := ""
	for _, node := range status.Nodes {
		if node.BoundaryID == "" {
			oldName = node.Name
		}
	}
	if oldName == "" {
		t.Fatal("captured workflow root missing")
	}
	raw, err = json.Marshal(status)
	compositionOK(t, err)
	var rebound wf.WorkflowStatus
	compositionOK(t, json.Unmarshal([]byte(strings.ReplaceAll(string(raw), oldName, workflowName)), &rebound))
	return rebound
}

func conditionalChange(t *testing.T, change string, w *wf.Workflow) []*core.Pod {
	t.Helper()
	for id, n := range w.Status.Nodes {
		if n.TemplateName != "build" {
			continue
		}
		switch change {
		case "error":
			n.Phase, n.Outputs = wf.NodeError, nil
		case "omitted-phase":
			n.Phase = wf.NodeOmitted
		case "skipped-error":
			n.Phase = wf.NodeError
		case "pod-type":
			n.Type = wf.NodeTypePod
		case "missing":
			delete(w.Status.Nodes, id)
			return nil
		case "pod":
			return []*core.Pod{nativePod(w, n, 0)}
		case "ambiguous-pods":
			first := nativePod(w, n, 0)
			second := first.DeepCopy()
			second.UID = "other-pod-uid"
			return []*core.Pod{first, second}
		case "other-failed":
			addConditionalFailure(t, w)
		}
		w.Status.Nodes[id] = n
		return nil
	}
	return nil
}

func conditionalAdmission(t *testing.T, tc conditionalEvidenceCase) *admissioncontract.PreparedAdmission {
	t.Helper()
	r := requestForProgress()
	document := conditionalDocument(t, tc)
	blob := strings.Repeat("b", 40)
	pipeline := account(t, identityname.PipelineCI, "")
	q := admissioncontract.PrepareRequest{Mode: admissioncontract.ModeSubmit, Repository: r.Repo, RunID: "conditional-evidence", Ref: r.Ref, ObjectSHA: r.ObjectSHA, CommitSHA: *r.CommitSHA, Trigger: r.Trigger, SelectedFile: ".oberth/build.yaml", FileBlob: blob, Document: document,
		SourceReceipt:   sourcecontract.Receipt{Version: 1, Revision: sourcecontract.Revision{Repository: r.Repo, Commit: *r.CommitSHA}, ObjectSHA: r.ObjectSHA, File: ".oberth/build.yaml", Blob: blob, DocumentDigest: sourcecontract.Digest(document)},
		IdentityRequest: admissioncontract.IdentityRequest{Namespace: "ci", PipelineAccount: pipeline}}
	e := argojob.New(argojob.Config{Accounts: nativefake.Accounts{Names: map[string]string{pipeline: "pipeline-uid"}}})
	p, err := argoworkflow.Prepare(t.Context(), q, argoworkflow.Policy{}, e.Hooks(nativebinding.Preparation{Repository: r.Repo, File: q.SelectedFile, Assist: !tc.unarmed, HelperImage: "oberth.ci/helper:1"}))
	compositionOK(t, err)
	return p
}

type conditionalTimer struct{}

// conditionalSource names the default branch, the one source fact a finish reads.
type conditionalSource struct{ service.Source }

func (conditionalSource) DefaultBranch(ctx context.Context, _ string) (string, error) {
	return "refs/heads/main", ctx.Err()
}

func (conditionalTimer) Stop() bool { return true }

func conditionalService(t *testing.T, s *store.Store, runs service.RunStore) *service.Service {
	t.Helper()
	c := service.Config{Runs: runs, Catalog: s, Events: s, Publications: s, Promotions: s, HoldStore: s,
		Authority: struct{ service.AuthorityStore }{}, MovingGit: struct{ service.MovingGit }{}, Holds: struct{ service.HoldExec }{},
		Git: struct{ service.Git }{}, Source: conditionalSource{}, Exec: struct{ service.Exec }{}, Starter: struct{ service.Starter }{}, Logs: struct{ service.Logs }{},
		Now: compositionClock, NewID: func() string { return "unused" }, OnError: func(err error) { t.Error(err) },
		AfterFunc: func(time.Duration, func()) service.Timer { return conditionalTimer{} }}
	svc, err := service.New(t.Context(), c)
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, svc.Shutdown(context.Background())) })
	return svc
}

const conditionalWorkflow = `apiVersion: argoproj.io/v1alpha1
kind: Workflow
spec:
  entrypoint: pipeline
  templates:
  - name: pipeline
    dag:
      tasks:
      - {name: build, template: build, when: 'false', continueOn: {failed: true}}
      - {name: other, template: build, when: 'false', continueOn: {failed: true}}
  - name: build
    container: {image: 'oberth.ci/build:1', command: ['true']}
`
