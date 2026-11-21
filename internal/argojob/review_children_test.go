package argojob_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/argojob"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// Kubernetes orphan propagation (cronsync/mutation.go:90) removes the deleted
// parent's ownerReference from every child. A proven child must stay observable.
func TestReviewProvenChildSurvivesOrphanPropagation(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, metaDeleteOptions()))
	f.child.OwnerReferences = nil // what the garbage collector does under DeletePropagationOrphan
	if _, err := f.executor().Adopt(t.Context(), proof.Run); err != nil {
		t.Fatal("proven child refused after orphan propagation:", err)
	}
}

// A finished child whose evidence is incomplete must not be re-adopted and
// re-reported on every later watch event (ordinary runs are not).
func TestReviewFinishedChildNotReportedTwice(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.Status.Phase = wf.WorkflowError // Argo refused the spec: no nodes, no evidence
	c := f.executorConfig()
	var mu sync.Mutex
	reports, failures := 0, 0
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		mu.Lock()
		reports++
		mu.Unlock()
		r, err := f.s.Run(ctx, id)
		if err != nil {
			return err
		}
		err = f.s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, NativePhase: &o.Phase, Reason: o.Reason, Failure: o.Failure})
		if err != nil {
			failures++
		}
		return err
	}}
	e := argojob.New(c)
	compositionOK(t, e.RecordChildForTest(t.Context(), f.child))
	for range 6 { // first pass observes and settles; later passes are resync events
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	if reports != 1 {
		t.Fatalf("finished child reported %d times, %d refused by the store", reports, failures)
	}
}

// Argo's Replace policy (and `argo stop`) set spec.shutdown on a child the
// server may not have recorded yet; discovery and observation both preserve this fact.
func TestReviewStoppedChildStillRecorded(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.Spec.Shutdown = wf.ShutdownStrategyTerminate
	if err := f.executor().RecordChildForTest(t.Context(), f.child); err != nil {
		t.Fatal("stopped child never becomes a run:", err)
	}
}

// Same UID seen by many concurrent observers: one run, all succeed.
func TestReviewConcurrentObserversOneRun(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- f.executor().RecordChildForTest(t.Context(), f.child.DeepCopy())
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		compositionOK(t, err)
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	compositionOK(t, err)
	if len(runs) != 1 {
		t.Fatal("concurrent observers created", len(runs))
	}
}

// A recreated object under a recorded name (new UID) is refused, never a second run.
func TestReviewSameNameNewUIDRefused(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	f.child.UID = types.UID("other-uid")
	if err := f.executor().RecordChildForTest(t.Context(), f.child); err == nil {
		t.Fatal("replacement UID accepted")
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	compositionOK(t, err)
	if len(runs) != 1 {
		t.Fatal("replacement UID created a run", len(runs))
	}
}

// Replay: after the parent moved to a new commit, a Workflow creator can still
// get the immediately previous admission recorded as a fresh scheduled run.
func TestReviewOldDigestReplayAfterParentMoved(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	old := f.child.DeepCopy()
	g := f.config.Git.(*cronfake.Git)
	commit := strings.Repeat("c", 40)
	g.PutRef("origin/team/repo", gitport.Ref{Name: "refs/heads/main", ObjectSHA: commit, CommitSHA: commit})
	g.PutFile(gitport.Revision{Repository: "origin/team/repo", Commit: commit}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("d", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	moved, err := f.native.Get(t.Context(), f.parent.Name, metaGetOptions())
	compositionOK(t, err)
	if moved.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation] == old.Annotations[nativebinding.AdmissionAnnotation] {
		t.Fatal("parent did not move")
	}
	forged := old.DeepCopy()
	forged.Name, forged.UID = f.parent.Name+"-999", "forged-uid"
	f.child = forged
	err = f.executor().RecordChildForTest(t.Context(), forged)
	t.Logf("old-digest replay with live parent UID: err=%v", err)
	if err != nil {
		t.Fatal("expected the previous admission to remain available")
	}
}

// An unknown admission must produce a visible diagnostic.
func TestReviewUnknownAdmissionIsReported(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.Annotations[nativebinding.AdmissionAnnotation] = strings.Repeat("e", 64)
	var log bytes.Buffer
	c := f.executorConfig()
	c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	e := argojob.New(c)
	for range 3 {
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	if !strings.Contains(log.String(), "level=") {
		t.Fatal("child waiting for an unknown admission is invisible")
	}
}

func metaDeleteOptions() meta.DeleteOptions { return meta.DeleteOptions{} }
func metaGetOptions() meta.GetOptions       { return meta.GetOptions{} }
