package argojob_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestScheduledChildRecordedOnceAcrossReplayAndRestart(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	for range 3 {
		compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	}
	compositionOK(t, f.s.Close())
	reopened, err := store.Open(t.Context(), f.path, store.DefaultOptions(compositionClock))
	compositionOK(t, err)
	defer func() { compositionOK(t, reopened.Close()) }()
	f.s = reopened
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	compositionOK(t, err)
	events, err := f.s.Events(t.Context(), stateport.EventFilter{Limit: 500})
	compositionOK(t, err)
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	retained, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if len(runs) != 1 || len(events.Events) != 1 || runs[0].Origin.Actor != "oberth:scheduler" || runs[0].Origin.ScheduleEntry != "nightly" || runs[0].ID != f.child.Name || proof.Run.SpecSHA256 != retained.Admission.SpecDigest {
		t.Fatal("child was not committed exactly once", runs, events, proof)
	}
}

// An accepted child keeps its sealed source roots under its own run ID, as a
// pushed run does before Create.
func TestScheduledChildKeepsItsSourceUnderItsRunID(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.config.Admit = func(_ context.Context, r cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		v := childAdmission(r)
		v.SourceBindings = []sourcecontract.Binding{{Path: "main", Revision: sourcecontract.Revision{Repository: r.Repository.Qualified, Commit: r.Revision.CommitSHA}}}
		p, err := admissioncontract.Seal(v)
		f.prepared = p
		return p, err
	}
	syncer, err := cronsync.New(f.config)
	compositionOK(t, err)
	f.syncer = syncer
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	f.parent, err = f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	f.child = f.fire(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	retained, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	kept := f.kept.owners()
	bindings, ok := kept[f.child.Name]
	if len(kept) != 1 || !ok || len(bindings) != 1 || !reflect.DeepEqual(bindings, retained.Admission.SourceBindings) {
		t.Fatal("accepted child did not keep its source", kept)
	}
}

type interruptedChildStore struct {
	*store.Store
	committed bool
}

func (s interruptedChildStore) AcceptBatch(ctx context.Context, b stateport.BatchAcceptance) (stateport.AcceptanceResult, error) {
	if s.committed {
		_, err := s.Store.AcceptBatch(ctx, b)
		if err != nil {
			return stateport.AcceptanceResult{}, err
		}
	}
	return stateport.AcceptanceResult{}, errors.New("observation interrupted")
}
func TestScheduledChildRestartBetweenObservationAndCommit(t *testing.T) {
	t.Parallel()
	for _, committed := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "lost_reply"}[committed], func(t *testing.T) {
			f := newChildFixture(t)
			c := f.executorConfig()
			c.ChildRuns = interruptedChildStore{f.s, committed}
			if err := argojob.New(c).RecordChildForTest(t.Context(), f.child); err == nil {
				t.Fatal("interruption ignored")
			}
			compositionOK(t, f.s.Close())
			s, err := store.Open(t.Context(), f.path, store.DefaultOptions(compositionClock))
			compositionOK(t, err)
			defer func() { compositionOK(t, s.Close()) }()
			f.s = s
			compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			runs, err := s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
			compositionOK(t, err)
			if len(runs) != 1 {
				t.Fatal("recovery did not retain one run", runs)
			}
		})
	}
}

func TestScheduledChildRefusesForgedAndMislabelledObjects(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*wf.Workflow){
		"owner":      func(w *wf.Workflow) { w.OwnerReferences[0].UID = "forged" },
		"repository": func(w *wf.Workflow) { w.Labels[nativebinding.RepositoryLabel] = "999" },
		"tier":       func(w *wf.Workflow) { w.Labels[nativebinding.TierLabel] = "release" },
		"commit":     func(w *wf.Workflow) { w.Labels[nativebinding.CommitLabel] = strings.Repeat("c", 40) },
		"spec":       func(w *wf.Workflow) { w.Spec.Templates[0].Container.Command = []string{"false"} },
	} {
		t.Run(name, func(t *testing.T) {
			f := newChildFixture(t)
			change(f.child)
			if err := f.executor().RecordChildForTest(t.Context(), f.child); err == nil {
				t.Fatal("forged child accepted")
			}
			runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
			compositionOK(t, err)
			if len(runs) != 0 {
				t.Fatal("refused child gained a run")
			}
		})
	}
}
func TestScheduledChildRefusalLoggedOncePerUID(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.OwnerReferences[0].UID = "forged"
	var log bytes.Buffer
	c := f.executorConfig()
	c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	e := argojob.New(c)
	for range 3 {
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	if strings.Count(log.String(), "level=ERROR") != 1 {
		t.Fatal("replayed refusal was not reported once", log.String())
	}
}

func TestScheduledChildEditedParentUsesFrozenAdmissionAndOrphanNeedsProof(t *testing.T) {
	t.Parallel()
	for _, recorded := range []bool{false, true} {
		t.Run(map[bool]string{false: "unproven_orphan", true: "proven_orphan"}[recorded], func(t *testing.T) {
			f := newChildFixture(t)
			parent := f.parent.DeepCopy()
			parent.Spec.WorkflowSpec.Templates[0].Container.Command = []string{"new"}
			_, err := f.native.Update(t.Context(), parent, meta.UpdateOptions{})
			compositionOK(t, err)
			if recorded {
				compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			}
			compositionOK(t, f.native.Delete(t.Context(), parent.Name, orphanOptions()))
			if len(f.child.OwnerReferences) != 0 {
				t.Fatal("orphan propagation retained its owner")
			}
			err = f.executor().RecordChildForTest(t.Context(), f.child)
			if recorded {
				compositionOK(t, err)
			} else if err == nil {
				t.Fatal("unproven orphan gained authority")
			}
			if recorded {
				changed := f.child.DeepCopy()
				changed.CreationTimestamp = meta.NewTime(compositionClock().Add(time.Second))
				if err := f.executor().RecordChildForTest(t.Context(), changed); err == nil {
					t.Fatal("durable proof ignored changed creation time")
				}
			}
		})
	}
}

func TestScheduledChildBeforeAdmissionCommitRetries(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.s = compositionStore(t)
	registerCompositionRepository(t, f.s, "origin/team/repo")
	e := f.executor()
	err := e.RecordChildForTest(t.Context(), f.child)
	if err == nil || publicerr.Permanent(err) {
		t.Fatal("missing sync commit became permanent refusal", err)
	}
	compositionOK(t, f.s.RetainSchedule(t.Context(), f.prepared))
	compositionOK(t, e.RecordChildForTest(t.Context(), f.child))
}

func TestScheduleWalkRecordsOneChildAndAdoptsSameUID(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	node := nativeNode("one", ".build", wf.NodeFailed, 1)
	node.Name = f.child.Name + ".build"
	f.child.Status.Phase = wf.WorkflowFailed
	f.child.Status.Nodes = wf.Nodes{node.ID: node}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c := f.executorConfig()
	c.Progress = nativeCompletionProbe{Store: f.s, done: cancel}
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		reports++
		r, err := f.s.Run(ctx, id)
		if err != nil {
			return err
		}
		return f.s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Failure: o.Failure})
	}}
	e := argojob.New(c)
	c.Cache.(*nativefake.Cache).PodObjects = []*core.Pod{nativePod(f.child, node, 1)}
	c.Cache.(*nativefake.Cache).Changed(f.child.Name)
	_ = e.Run(ctx)
	r, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	steps, err := f.s.Steps(t.Context(), f.child.Name)
	compositionOK(t, err)
	if reports != 1 || !r.EvidenceComplete || r.Origin.WorkflowUID != string(f.child.UID) || r.Failed != 1 || len(steps) != 1 || len(steps[0].Logs) != 1 {
		t.Fatal("watch did not record ordinary run evidence", r)
	}
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, orphanOptions()))
	if len(f.child.OwnerReferences) != 0 {
		t.Fatal("orphan propagation retained its owner")
	}
	adopted, err := f.executor().Adopt(t.Context(), proof.Run)
	compositionOK(t, err)
	if adopted.UID != f.child.UID {
		t.Fatal("restart replaced child UID")
	}
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	if c.Workflows.(*nativefake.Workflows).Posts != 0 {
		t.Fatal("discovery submitted a Workflow")
	}
}

var _ argojob.Children = (*cronsync.Syncer)(nil)

type duringChildPublication struct {
	cronsync.Native
	published func(*wf.CronWorkflow)
}

func (n duringChildPublication) Update(ctx context.Context, w *wf.CronWorkflow, o meta.UpdateOptions) (*wf.CronWorkflow, error) {
	out, err := n.Native.Update(ctx, w, o)
	if err == nil {
		n.published(out)
	}
	return out, err
}
func TestScheduledChildSeenBeforeSyncReturns(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	config := f.config
	observed := false
	config.Client = duringChildPublication{Native: f.native, published: func(parent *wf.CronWorkflow) {
		f.parent = parent
		f.child = f.fire(t)
		compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
		observed = true
	}}
	parent := f.parent.DeepCopy()
	parent.Spec.Suspend = true
	_, err := f.native.Update(t.Context(), parent, meta.UpdateOptions{})
	compositionOK(t, err)
	f.syncer, err = cronsync.New(config)
	compositionOK(t, err)
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	if !observed {
		t.Fatal("child was not observed during native publication")
	}
}

func TestManualScheduleRunUsesOrdinaryBinding(t *testing.T) {
	t.Parallel()
	s := compositionStore(t)
	r := requestForProgress()
	r.Trigger = "schedule"
	r.ScheduleEntry = "nightly"
	registerCompositionRepository(t, s, r.Repo)
	p := compositionAdmission(t, "manual-schedule", r)
	_, err := s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	native := &nativefake.Workflows{}
	e := argojob.New(argojob.Config{Runs: s, Workflows: native})
	b, err := e.Create(t.Context(), p)
	compositionOK(t, err)
	_, err = e.Adopt(t.Context(), b)
	compositionOK(t, err)
}

func TestScheduledChildWithoutCronLabelIsIgnored(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	delete(f.child.Labels, nativebinding.NativeCronLabel)
	f.executor().ProcessForTest(t.Context(), f.child.Name)
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	compositionOK(t, err)
	if len(runs) != 0 {
		t.Fatal("ordinary object discovered as a scheduled run")
	}
}
