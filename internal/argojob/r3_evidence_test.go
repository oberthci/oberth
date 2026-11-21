package argojob_test

import (
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/stateport"
	statefake "github.com/oberthci/oberth/internal/stateport/fake"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	"testing"
	"time"
)

func TestEvidenceExhaustionInvalidatesPreviouslyCompleteFacts(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	owner := c.Runs.(*statefake.Runs)
	n := nativeNode("n", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Phase = wf.WorkflowFailed
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	c.Cache = &fake.Cache{WorkflowObject: f.Object, PodObjects: []*core.Pod{nativePod(f.Object, n, 7)}}
	o := project(t, argojob.Projection{Workflow: f.Object, Plan: ownerPlan(t, owner, b.RunID), Pods: c.Cache.Pods(b.Workflow.Name)})
	seq, err := owner.BeginEvidence(t.Context(), stateport.Action{}, stateport.EvidenceStart{RunID: b.RunID, Nodes: len(o.Nodes)})
	compositionOK(t, err)
	compositionOK(t, owner.PutNodes(t.Context(), stateport.Action{}, stateport.NodeBatch{RunID: b.RunID, Sequence: seq, Nodes: o.Nodes}))
	compositionOK(t, owner.CompleteEvidence(t.Context(), stateport.Action{}, stateport.EvidenceCompletion{RunID: b.RunID, Sequence: seq}))
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	c.Metrics = &argojob.Metrics{}
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	calls := 0
	c.Progress = &progressProbe{Runs: owner, begin: func(context.Context, stateport.Action, stateport.EvidenceStart) (int64, error) {
		calls++
		return 0, errors.New("evidence refused")
	}, advance: func(ctx context.Context, id string) error { err := owner.AdvanceNative(ctx, id); cancel(); return err }}
	_ = argojob.New(c).Run(ctx)
	row, err := owner.Run(t.Context(), b.RunID)
	compositionOK(t, err)
	if calls != 5 || row.EvidenceComplete || c.Metrics.IncompleteEvidence.Load() != 1 {
		t.Fatal("exhaustion retained complete evidence", calls, row.EvidenceComplete)
	}
}
func ownerPlan(t *testing.T, owner *statefake.Runs, id string) admissioncontract.Plan {
	t.Helper()
	row, err := owner.Run(t.Context(), id)
	compositionOK(t, err)
	return row.Admission.Plan
}

type initiallyWarmCache struct {
	*fake.Cache
	reads int
}

func (c *initiallyWarmCache) Workflow(name string) (*wf.Workflow, bool) {
	c.reads++
	if c.reads == 1 {
		return c.Cache.Workflow(name)
	}
	return nil, false
}

type recoveringRead struct {
	*fake.Workflows
	now       *time.Time
	recovered time.Time
	reads     int
}

func (f *recoveringRead) Get(ctx context.Context, name string, o meta.GetOptions) (*wf.Workflow, error) {
	f.reads++
	if f.now.Before(f.recovered) {
		return nil, errors.New("API disconnected")
	}
	return f.Workflows.Get(ctx, name, o)
}
func TestAcknowledgedTerminalAPIErrorsDoNotConsumeEvidenceRefusals(t *testing.T) {
	t.Parallel()
	c, f, b := observationFixture(t)
	owner := c.Runs.(*statefake.Runs)
	n := nativeNode("n", "", wf.NodeFailed, 1)
	n.Name = b.Workflow.Name
	f.Object.Status.Nodes = wf.Nodes{n.ID: n}
	f.Object.Status.Phase = wf.WorkflowFailed
	cache := &initiallyWarmCache{Cache: &fake.Cache{WorkflowObject: f.Object}}
	c.Cache = cache
	now := time.Unix(100, 0)
	c.Now = func() time.Time { return now }
	native := &recoveringRead{Workflows: f, now: &now, recovered: now.Add(20 * time.Second)}
	c.Workflows = native
	c.RateLimiter = &advancingBackoff{TypedRateLimiter: workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 8*time.Second), now: &now}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	calls, invalidated := 0, 0
	c.Progress = &progressProbe{Runs: owner, begin: func(ctx context.Context, a stateport.Action, in stateport.EvidenceStart) (int64, error) {
		calls++
		seq, err := owner.BeginEvidence(ctx, a, in)
		if calls == 1 {
			cache.Changed(b.Workflow.Name)
		} else {
			cancel()
		}
		return seq, err
	}, advance: func(context.Context, string) error { invalidated++; cancel(); return nil }}
	_ = argojob.New(c).Run(ctx)
	if calls != 2 || native.reads < 6 || invalidated != 0 {
		t.Fatal("API outage consumed evidence refusals", calls, native.reads, invalidated)
	}
}
