package argojob_test

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/cronsync"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
)

func TestSecondReviewUnchangedRetryKeepsActualPredecessor(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	oldest := f.prepared.View().Digest
	c := f.config
	c.PublishAdmission = func(context.Context, int64, string, []string) error {
		return errors.New("publication callback interrupted before commit")
	}
	var err error
	f.syncer, err = cronsync.New(c)
	compositionOK(t, err)
	advanceChildRef(f, "b")
	if err := f.syncer.SyncAll(t.Context()); err == nil {
		t.Fatal("missing publication interruption")
	}
	previous := f.prepared.View().Digest
	f.parent, err = f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if f.parent.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation] != previous {
		t.Fatal("B was not published natively")
	}
	f.child = f.fire(t) // The real predecessor fires before C replaces it.
	advanceChildRef(f, "c")
	if err := f.syncer.SyncAll(t.Context()); err == nil {
		t.Fatal("missing second publication interruption")
	}
	current := f.prepared.View().Digest
	live, err := f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if live.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation] != current {
		t.Fatal("C was not published natively")
	}
	f.syncer, err = cronsync.New(f.config) // Restart; C is unchanged and the store is healthy.
	compositionOK(t, err)
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	_, oldErr := f.s.ScheduleAdmission(t.Context(), 1, "nightly", oldest)
	_, previousErr := f.s.ScheduleAdmission(t.Context(), 1, "nightly", previous)
	_, currentErr := f.s.ScheduleAdmission(t.Context(), 1, "nightly", current)
	recordErr := f.executor().RecordChildForTest(t.Context(), f.child)
	t.Logf("A=%v B=%v C=%v B-child=%v", oldErr, previousErr, currentErr, recordErr)
	if previousErr != nil || currentErr != nil || recordErr != nil || oldErr == nil {
		t.Fatal("unchanged retry must retain C/B, expire A, and record B's child")
	}
}

func secondRefused(t *testing.T, f *childFixture, e *argojob.Executor) {
	t.Helper()
	err := e.RecordChildForTest(t.Context(), f.child)
	if err == nil || !publicerr.Permanent(err) {
		t.Fatalf("binding must fail closed, got %v", err)
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	compositionOK(t, err)
	if len(runs) != 0 {
		t.Fatal("refused binding wrote a run")
	}
}

// External evidence shapes are changed, not only the Go checks that consume them.
func TestSecondReviewControlParentManagement(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing", "empty", "replaced"} {
		t.Run(shape, func(t *testing.T) {
			f := newChildFixture(t)
			p := f.parent.DeepCopy()
			switch shape {
			case "missing":
				p.Labels = nil
			case "empty":
				p.Labels[nativebinding.ManagedLabel] = ""
			case "replaced":
				p.Labels[nativebinding.ManagedLabel] = "somebody-else"
			}
			_, err := f.native.Update(t.Context(), p, meta.UpdateOptions{})
			compositionOK(t, err)
			secondRefused(t, f, f.executor())
		})
	}
}

func TestSecondReviewControlOwnerGroup(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.OwnerReferences[0].APIVersion = "elsewhere.ci/v1"
	secondRefused(t, f, f.executor())
}

func TestSecondReviewControlNamespace(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	c := f.config
	c.Namespace = "other-pipeline"
	var err error
	f.syncer, err = cronsync.New(c)
	compositionOK(t, err)
	secondRefused(t, f, f.executor())
}

type secondChangedAdmission struct {
	argojob.ChildRuns
	field string
}

func (s secondChangedAdmission) ScheduleAdmission(ctx context.Context, id int64, entry, digest string) (admissioncontract.PreparedView, error) {
	v, err := s.ChildRuns.ScheduleAdmission(ctx, id, entry, digest)
	if err != nil {
		return v, err
	}
	switch s.field {
	case "plan":
		v.Plan.Entries[0].Name = "not-admitted"
	case "grants":
		v.Grants = append(v.Grants, admissioncontract.GrantEvidence{})
	case "templates":
		v.Templates = append(v.Templates, admissioncontract.TemplateSnapshot{Name: "not-admitted"})
	}
	return v, nil
}

func TestSecondReviewControlSealedAuthority(t *testing.T) {
	t.Parallel()
	for _, field := range []string{"plan", "grants", "templates"} {
		t.Run(field, func(t *testing.T) {
			f := newChildFixture(t)
			c := f.executorConfig()
			c.ChildRuns = secondChangedAdmission{ChildRuns: f.s, field: field}
			secondRefused(t, f, argojob.New(c))
		})
	}
}

func TestSecondReviewControlOrphanUID(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, orphanOptions()))
	f.child.UID = "replacement-uid"
	err := f.executor().RecordChildForTest(t.Context(), f.child)
	if err == nil || !publicerr.Permanent(err) {
		t.Fatalf("replacement orphan acquired durable custody: %v", err)
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	compositionOK(t, err)
	if len(runs) != 1 || runs[0].Origin.WorkflowUID != "child-uid" {
		t.Fatal("replacement changed original receipt")
	}
}

func TestSecondReviewExternalOwnerShapes(t *testing.T) {
	t.Parallel()
	for _, shape := range []string{"missing", "noncontroller", "foreign-kind", "missing-parent", "recreated-parent"} {
		t.Run(shape, func(t *testing.T) {
			f := newChildFixture(t)
			switch shape {
			case "missing":
				f.child.OwnerReferences = nil
			case "noncontroller":
				f.child.OwnerReferences[0].Controller = new(false)
			case "foreign-kind":
				f.child.OwnerReferences[0].Kind = "Workflow"
			case "missing-parent", "recreated-parent":
				// Keep the old owner ref to test missing/replaced parent lookup.
				w := f.child.DeepCopy()
				compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, orphanOptions()))
				f.child = w
				if shape == "recreated-parent" {
					compositionOK(t, f.syncer.SyncAll(t.Context()))
				}
			}
			secondRefused(t, f, f.executor())
		})
	}
}

// This is an authority characterization, not a claim that a step can create objects.
func TestSecondReviewUnpublishedAdmissionCanBeSelected(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	c := f.config
	c.Client = failingUpdate{f.native}
	failed, err := cronsync.New(c)
	compositionOK(t, err)
	advanceChildRef(f, "b")
	if err := failed.SyncAll(t.Context()); err == nil {
		t.Fatal("update must fail")
	}
	v := f.prepared.View()
	live, err := f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if live.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation] == v.Digest {
		t.Fatal("fixture accidentally published B")
	}
	// A Workflow creator can supply B's spec/metadata while naming live A's UID.
	f.child.Spec = *v.Workflow.Spec.DeepCopy()
	f.child.Labels[nativebinding.CommitLabel] = strings.Repeat("b", 40)
	f.child.Annotations[nativebinding.AdmissionAnnotation] = v.Digest
	f.child.Annotations[nativebinding.IdentityAnnotation] = v.SpecDigest
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	r, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if r.Admission.CommitSHA != strings.Repeat("b", 40) {
		t.Fatal("wrong characterization result")
	}
}

func TestSecondReviewBackupRestoresAdmissionsAndCustody(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	previous := f.prepared.View().Digest
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	current := f.prepared.View().Digest
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	var archive bytes.Buffer
	_, err := f.s.Snapshot(t.Context(), options, &archive)
	compositionOK(t, err)
	b, err := store.NewBackup(f.path)
	compositionOK(t, err)
	destination := filepath.Join(t.TempDir(), "restored.sqlite")
	_, err = b.Restore(t.Context(), stateport.RestoreRequest{Options: options, Destination: destination}, bytes.NewReader(archive.Bytes()))
	compositionOK(t, err)
	restored, err := store.Open(t.Context(), destination, store.DefaultOptions(compositionClock))
	compositionOK(t, err)
	t.Cleanup(func() { compositionOK(t, restored.Close()) })
	f.s = restored
	for _, digest := range []string{previous, current} {
		_, err = restored.ScheduleAdmission(t.Context(), 1, "nightly", digest)
		compositionOK(t, err)
	}
	// An unchanged publication supplies the predecessor retained on the native parent.
	compositionOK(t, restored.PublishSchedule(t.Context(), 1, "nightly", []string{current, previous}))
	_, err = restored.ScheduleAdmission(t.Context(), 1, "nightly", previous)
	compositionOK(t, err)
	compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, orphanOptions()))
	compositionOK(t, restored.ForgetSchedule(t.Context(), 1, "nightly"))
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	runs, err := restored.Runs(t.Context(), stateport.RunFilter{Limit: 20})
	compositionOK(t, err)
	events, err := restored.Events(t.Context(), stateport.EventFilter{Limit: 20})
	compositionOK(t, err)
	if len(runs) != 1 || len(events.Events) != 1 {
		t.Fatal("restore duplicated custody", len(runs), len(events.Events))
	}
}

func TestSecondReviewForeignRepositoryAdmissionRefused(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	registerCompositionRepository(t, f.s, "origin/team/other")
	v := f.prepared.View()
	v.Repository = "origin/team/other"
	foreign, err := admissioncontract.Seal(v)
	compositionOK(t, err)
	compositionOK(t, f.s.RetainSchedule(t.Context(), foreign))
	if foreign.View().SpecDigest != f.prepared.View().SpecDigest {
		t.Fatal("control requires identical native specs")
	}
	f.child.Annotations[nativebinding.AdmissionAnnotation] = foreign.View().Digest
	secondRefused(t, f, f.executor())
}

type secondTransientProof struct {
	argojob.ChildRuns
	calls int
}

func (s *secondTransientProof) ChildProof(ctx context.Context, id string) (nativebinding.ChildBinding, error) {
	s.calls++
	if s.calls == 1 {
		return nativebinding.ChildBinding{}, publicerr.New(publicerr.Unavailable, "temporary proof read failure", "Retry the read.", nil)
	}
	return s.ChildRuns.ChildProof(ctx, id)
}

func TestSecondReviewRecoveryRetriesChildProofWithoutWatchEvent(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	c := f.executorConfig()
	proofs := &secondTransientProof{ChildRuns: f.s}
	c.ChildRuns = proofs
	// The child disappeared while the server was down: initial LIST is empty.
	c.Cache = &nativefake.Cache{}
	c.Workflows = &nativefake.Workflows{}
	c.RateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Nanosecond, time.Nanosecond)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		r, err := f.s.Run(ctx, id)
		if err != nil {
			return err
		}
		err = f.s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, Reason: o.Reason})
		if err == nil {
			reports++
			cancel()
		}
		return err
	}}
	_ = argojob.New(c).Run(ctx)
	r, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if proofs.calls < 2 || reports != 1 || r.Status != "failed" {
		t.Fatalf("transient startup proof failure stranded child: proof reads=%d reports=%d status=%s", proofs.calls, reports, r.Status)
	}
}
