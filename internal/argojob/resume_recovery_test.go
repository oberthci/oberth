package argojob_test

import (
	"context"
	"encoding/json"
	"fmt"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"slices"
	"testing"
	"time"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	"github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/pkg/argoworkflow"
)

func TestResumeRestartBeforeApplyAndAfterRelabel(t *testing.T) {
	t.Parallel()
	s, in, native, pods := resumeRecoveryFixture(t)
	config := resumeRecoveryConfig(s, native, pods)
	compositionOK(t, argojob.New(config).ApplyResume(t.Context(), in.RunID))
	if native.Posts != 0 || native.Object.Labels[nativebinding.RunLabel] != in.RunID {
		t.Fatal("recovery created or lost Workflow")
	}
	// A second process starts after the labels moved. It must observe R2,
	// finish its record, and release the finalizer through R2's native UID.
	native.Object.Status = wf.WorkflowStatus{Phase: wf.WorkflowSucceeded}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	finished := ""
	collected := false
	config.Workflows = collectionNative{Workflows: native, collected: func(*wf.Workflow) { collected = true; cancel() }}
	config.Logs = fake.Logs{ArchiveFunc: func(context.Context, string, []runlog.ContainerLog) (bool, error) { return true, nil }}
	config.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		finished = id
		row, err := s.Run(ctx, id)
		if err != nil {
			return err
		}
		return s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: row.Revision, Status: o.Status})
	}}
	_ = argojob.New(config).Run(ctx)
	if finished != in.RunID || !collected || slices.Contains(native.Object.Finalizers, nativebinding.RecordFinalizer) {
		t.Fatal("resumed owner lost after restart", finished, collected, ctx.Err())
	}
}

type resumeRecoveryPods struct {
	*fake.Pods
	objects      []*core.Pod
	beforeDelete func()
}

func (p *resumeRecoveryPods) List(ctx context.Context, _ meta.ListOptions) (*core.PodList, error) {
	list := &core.PodList{}
	for _, pod := range p.objects {
		list.Items = append(list.Items, *pod.DeepCopy())
	}
	return list, ctx.Err()
}
func (p *resumeRecoveryPods) Delete(ctx context.Context, name string, o meta.DeleteOptions) error {
	for i, pod := range p.objects {
		if pod.Name == name {
			if o.Preconditions == nil || o.Preconditions.UID == nil || *o.Preconditions.UID != pod.UID {
				return fmt.Errorf("UID precondition missing")
			}
			p.objects = append(p.objects[:i], p.objects[i+1:]...)
			p.Deletes++
			if p.beforeDelete != nil {
				p.beforeDelete()
			}
			return ctx.Err()
		}
	}
	return nil
}

func resumeRecoveryFixture(t *testing.T) (*store.Store, argojob.ResumeInput, *fake.Workflows, *resumeRecoveryPods) {
	t.Helper()
	in := resumeFixture(t, resumeDAG)
	plan, err := argojob.PlanResume(t.Context(), in)
	compositionOK(t, err)
	s := compositionStore(t)
	r := requestForProgress()
	registerCompositionRepository(t, s, r.Repo)
	p, err := admissioncontract.Seal(in.Sealed)
	compositionOK(t, err)
	_, err = s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: p}}})
	compositionOK(t, err)
	compositionOK(t, s.BindWorkflow(t.Context(), stateport.Action{Actor: r.Actor.Identity}, in.Predecessor))
	old, err := s.Run(t.Context(), in.Predecessor.RunID)
	compositionOK(t, err)
	compositionOK(t, s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: old.ID, Revision: old.Revision, Status: stepmodel.RunFailed}))
	r.CommitSHA = &in.Commit
	r.ObjectSHA = in.Commit
	r.ReceiveID = "resume"
	fresh := compositionDocument(t, admissionInput{id: in.RunID, request: r, document: []byte(resumeDAG)})
	fresh, err = argoworkflow.ResumeAdmission(fresh, in.Sealed)
	compositionOK(t, err)
	body, err := json.Marshal(plan)
	compositionOK(t, err)
	_, err = s.AcceptBatch(t.Context(), stateport.BatchAcceptance{ReceiveID: r.ReceiveID, Repository: r.Repo, Actor: r.Actor, Runs: []stateport.Acceptance{{Request: r, Prepared: fresh, Resume: &model.RunOrigin{ResumedFrom: old.ID, ResumeUID: old.Origin.NativeUID(), WorkflowName: old.Origin.WorkflowName, Assisted: true, ResumePlan: body}}}})
	compositionOK(t, err)
	native := &fake.Workflows{Object: in.Workflow.DeepCopy()}
	pods := &resumeRecoveryPods{Pods: &fake.Pods{}, objects: in.Pods}
	return s, in, native, pods
}

func resumeRecoveryConfig(s *store.Store, native *fake.Workflows, pods *resumeRecoveryPods) argojob.Config {
	return argojob.Config{Runs: s, Progress: s, Workflows: native, Pods: pods, Cache: &fake.Cache{}, Now: resumeNow, Reporter: &reportSpy{}}
}
