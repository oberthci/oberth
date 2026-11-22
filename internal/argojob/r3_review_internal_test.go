package argojob

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"io"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"log/slog"
	"strings"
	"testing"
	"time"
)

type reviewR2Native struct {
	Workflows
	w      *wf.Workflow
	outage bool
	reads  int
}

func (n *reviewR2Native) Get(context.Context, string, meta.GetOptions) (*wf.Workflow, error) {
	n.reads++
	if n.outage {
		return nil, errors.New("API disconnected")
	}
	return n.w.DeepCopy(), nil
}
func (n *reviewR2Native) Workflow(string) (*wf.Workflow, bool) { return nil, false }
func (n *reviewR2Native) Pods(string) []*core.Pod              { return nil }
func (n *reviewR2Native) OnChange(func(string))                {}

type reviewR2Progress struct {
	ProgressStore
	invalidated int
}

func (p *reviewR2Progress) SaveProgress(context.Context, string, stateport.Progress) error {
	return nil
}
func (p *reviewR2Progress) BeginEvidence(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error) {
	return 1, nil
}
func (p *reviewR2Progress) PutNodes(context.Context, stateport.Action, stateport.NodeBatch) error {
	return nil
}
func (p *reviewR2Progress) CompleteEvidence(context.Context, stateport.Action, stateport.EvidenceCompletion) error {
	return nil
}
func (p *reviewR2Progress) AdvanceNative(context.Context, string) error { p.invalidated++; return nil }

type reviewR2Reporter struct{ calls, refuse int }

func (r *reviewR2Reporter) Finished(context.Context, string, Observation) error {
	r.calls++
	if r.calls <= r.refuse {
		return errors.New("service temporarily unavailable")
	}
	return nil
}
func (r *reviewR2Reporter) Held(context.Context, model.HeldNotice) error { return nil }
func reviewR2Executor(t *testing.T, reporter *reviewR2Reporter) (*Executor, *tracking, *reviewR2Native, *time.Time) {
	t.Helper()
	now := time.Unix(100, 0)
	b := nativebinding.RunBinding{Workflow: nativebinding.Object{Namespace: "ci", Name: "run", UID: "uid"}, RunID: "id", RepositoryID: 1, Ref: "refs/heads/topic", Trigger: "branch", Revision: sourcecontract.Revision{Repository: "forge/org/repo", Commit: strings.Repeat("a", 40)}, Identity: strings.Repeat("b", 64), SpecSHA256: strings.Repeat("c", 64)}
	v := admissioncontract.PreparedView{RunID: b.RunID, Ref: b.Ref, CommitSHA: b.Revision.Commit, SpecDigest: b.SpecSHA256, Trigger: "branch"}
	w := &wf.Workflow{ObjectMeta: meta.ObjectMeta{Name: b.Workflow.Name, Namespace: b.Workflow.Namespace, UID: "uid"}, Status: wf.WorkflowStatus{Phase: wf.WorkflowSucceeded, Nodes: wf.Nodes{"n": {ID: "n", Name: "run.build", Type: wf.NodeTypePod, Phase: wf.NodeSucceeded}}}}
	stampSubmission(w, v, b.RepositoryID)
	native := &reviewR2Native{w: w}
	e := New(Config{Workflows: native, Cache: native, Progress: &reviewR2Progress{}, Reporter: reporter, Now: func() time.Time { return now }, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	t.Cleanup(e.queue.ShutDown)
	e.track(b, admissioncontract.Plan{Entries: []admissioncontract.PlanEntry{{Path: "build"}}}, trackingSeed{})
	return e, e.tracked[b.Workflow.Name], native, &now
}
func TestReviewR2TerminalPublicationSurvivesFiveRefusals(t *testing.T) {
	t.Parallel()
	reporter := &reviewR2Reporter{refuse: 5}
	e, tr, _, _ := reviewR2Executor(t, reporter)
	for range 10 {
		e.process(t.Context(), tr.binding.Workflow.Name)
	}
	if !tr.completed {
		t.Fatalf("terminal never delivered after service recovered: reports=%d tracked=%d invalidations=%d", reporter.calls, len(e.tracked), e.config.Progress.(*reviewR2Progress).invalidated)
	}
}
func TestReviewR2TerminalOutageHonorsEvidenceDeadline(t *testing.T) {
	t.Parallel()
	reporter := &reviewR2Reporter{}
	e, tr, native, now := reviewR2Executor(t, reporter)
	e.process(t.Context(), tr.binding.Workflow.Name)
	if !tr.completed || tr.evidenceUntil.IsZero() {
		t.Fatal("fixture failed to enter evidence collection")
	}
	native.outage = true
	*now = now.Add(2 * time.Minute)
	for range 20 {
		e.process(t.Context(), tr.binding.Workflow.Name)
	}
	if len(e.tracked) != 0 {
		t.Fatalf("terminal run retained beyond evidence window: deadline=%s now=%s reads=%d reports=%d", tr.evidenceUntil, *now, native.reads, reporter.calls)
	}
}
func TestReviewR2ExpiredPendingCaptureNotDispatched(t *testing.T) {
	t.Parallel()
	e, tr, _, now := reviewR2Executor(t, &reviewR2Reporter{})
	tr.completed = true
	tr.evidenceUntil = now.Add(time.Minute)
	e.offerCapture(tr, captureJob{binding: tr.binding, entries: []runlog.ContainerLog{{Step: "build", PodUID: "pod", Container: "main"}}})
	*now = now.Add(2 * time.Minute)
	if _, ok := e.nextCapture(); ok {
		t.Fatal("expired pending job starts a fresh 30-second capture beyond evidence window")
	}
}
