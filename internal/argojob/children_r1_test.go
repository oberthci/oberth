package argojob_test

import (
	"bytes"
	"context"
	"errors"
	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/admissioncontract"
	"github.com/oberthci/oberth/internal/argojob"
	nativefake "github.com/oberthci/oberth/internal/argojob/fake"
	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/nativebinding"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
	"github.com/oberthci/oberth/internal/store"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestRecordedChildSpecChangeRefused(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	f.child.Spec.Templates[0].Container.Command = []string{"changed"}
	if _, err = f.executor().Adopt(t.Context(), proof.Run); err == nil {
		t.Fatal("edited recorded spec accepted")
	}
}

func TestRecordedChildBoundaryChecks(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*childFixture){
		"repository": func(f *childFixture) { f.child.Labels[nativebinding.RepositoryLabel] = "999" },
		"namespace":  func(f *childFixture) { f.child.Namespace = "other" },
		"cron_label": func(f *childFixture) { f.child.Labels[nativebinding.NativeCronLabel] = "other" },
		"admission_annotation": func(f *childFixture) {
			f.child.Annotations[nativebinding.AdmissionAnnotation] = strings.Repeat("f", 64)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newChildFixture(t)
			compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			proof, err := f.s.ChildProof(t.Context(), f.child.Name)
			compositionOK(t, err)
			compositionOK(t, f.native.Delete(t.Context(), f.parent.Name, orphanOptions()))
			change(f)
			if _, err = f.executor().Adopt(t.Context(), proof.Run); err == nil {
				t.Fatal("recorded orphan drift accepted")
			}
		})
	}
}

func TestRecordedChildRequiresConfiguredNamespace(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	config := f.config
	config.Namespace = "other"
	verifier, err := cronsync.New(config)
	compositionOK(t, err)
	c := f.executorConfig()
	c.Children = verifier
	if _, err = argojob.New(c).Adopt(t.Context(), proof.Run); err == nil {
		t.Fatal("foreign namespace configuration accepted")
	}
}

func TestChildAcceptanceFactMismatchesArePermanent(t *testing.T) {
	t.Parallel()
	for name, change := range map[string]func(*nativebinding.ChildBinding){
		"repository": func(p *nativebinding.ChildBinding) { p.Run.Revision.Repository = "origin/team/other" },
		"ref":        func(p *nativebinding.ChildBinding) { p.Run.Ref = "refs/heads/other" },
		"commit":     func(p *nativebinding.ChildBinding) { p.Run.Revision.Commit = strings.Repeat("f", 40) },
		"entry":      func(p *nativebinding.ChildBinding) { p.ScheduleEntry = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			f := newChildFixture(t)
			proof, found, _, err := f.syncer.ChildBinding(t.Context(), f.child)
			compositionOK(t, err)
			if !found {
				t.Fatal("fixture not a child")
			}
			p, err := f.prepared.View().ScheduledChild(f.child)
			compositionOK(t, err)
			proof.Run.Identity, proof.Run.SpecSHA256 = p.View().Digest, p.View().SpecDigest
			change(&proof)
			err = f.executor().AcceptChildForTest(t.Context(), p, proof)
			if err == nil || !publicerr.Permanent(err) {
				t.Fatal("binding mismatch became a retry", err)
			}
		})
	}
}

type countedChildren struct {
	argojob.Children
	calls *int
}

func (c countedChildren) ChildBinding(ctx context.Context, w *wf.Workflow) (nativebinding.ChildBinding, bool, bool, error) {
	*c.calls++
	return c.Children.ChildBinding(ctx, w)
}

func TestChildQueueRetriesAndPrunesRefusals(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.s = compositionStore(t)
	registerCompositionRepository(t, f.s, "origin/team/repo")
	c := f.executorConfig()
	calls := 0
	c.Children = countedChildren{Children: c.Children, calls: &calls}
	var log bytes.Buffer
	c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	e := argojob.New(c)
	e.ProcessForTest(t.Context(), f.child.Name)
	if e.ChildRetriesForTest(f.child.Name) != 1 || !strings.Contains(log.String(), "scheduled admission not found") {
		t.Fatal("uncommitted admission was not visibly requeued", log.String())
	}
	compositionOK(t, f.s.RetainSchedule(t.Context(), f.prepared))
	e.ProcessForTest(t.Context(), f.child.Name)
	if e.ChildRetriesForTest(f.child.Name) != 0 || e.ChildNoticesForTest() != 0 {
		t.Fatal("successful retry was not forgotten")
	}
	// A different native UID with bad custody is a permanent, cache-bounded refusal.
	cache := c.Cache.(*nativefake.Cache)
	f.child = f.child.DeepCopy()
	f.child.Name += "9"
	f.child.UID = "refused"
	f.child.OwnerReferences[0].UID = "other"
	cache.WorkflowObject = f.child
	e.ProcessForTest(t.Context(), f.child.Name)
	if retries := e.ChildRetriesForTest(f.child.Name); retries != 0 {
		t.Fatal("permanent child was requeued", retries)
	}
	before := calls
	for range 3 {
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	if calls != before || e.ChildRetriesForTest(f.child.Name) != 0 || e.ChildNoticesForTest() != 1 {
		t.Fatal("permanent child was retried or retained incorrectly", calls, before)
	}
	cache.WorkflowObject = nil
	cache.Changed(f.child.Name)
	if e.ChildNoticesForTest() != 0 {
		t.Fatal("deleted child retained refusal memory")
	}
}

type proofReadError struct {
	argojob.ChildRuns
	err error
}

func (p proofReadError) ChildProof(context.Context, string) (nativebinding.ChildBinding, error) {
	return nativebinding.ChildBinding{}, p.err
}
func TestChildProofOutageDoesNotGrantNewCustody(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	c := f.executorConfig()
	cause := publicerr.New(publicerr.Unavailable, "store unavailable", "Restore store access.", nil)
	c.ChildRuns = proofReadError{c.ChildRuns, cause}
	calls := 0
	c.Children = countedChildren{c.Children, &calls}
	if err := argojob.New(c).RecordChildForTest(t.Context(), f.child); !errors.Is(err, cause) || calls != 0 {
		t.Fatal("proof read outage became absence", err, calls)
	}
}

type countedChildStore struct {
	*store.Store
	proofs, runs int
}

func (s *countedChildStore) ChildProof(ctx context.Context, id string) (nativebinding.ChildBinding, error) {
	s.proofs++
	return s.Store.ChildProof(ctx, id)
}
func (s *countedChildStore) Run(ctx context.Context, id string) (stateport.Run, error) {
	s.runs++
	return s.Store.Run(ctx, id)
}
func TestTrackedChildObservationUsesRetainedProof(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	s := &countedChildStore{Store: f.s}
	c := f.executorConfig()
	c.ChildRuns = s
	c.Runs = s
	e := argojob.New(c)
	compositionOK(t, e.RecordChildForTest(t.Context(), f.child))
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	for range 3 {
		_, err = e.Adopt(t.Context(), proof.Run)
		compositionOK(t, err)
	}
	if s.proofs != 1 || s.runs != 1 {
		t.Fatal("tracked observation repeated durable reads", s.proofs, s.runs)
	}
}

func TestChildEventUsesObservationTime(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	now := compositionClock().Add(24 * time.Hour)
	c := f.executorConfig()
	c.Now = func() time.Time { return now }
	compositionOK(t, argojob.New(c).RecordChildForTest(t.Context(), f.child))
	page, err := f.s.Events(t.Context(), stateport.EventFilter{Limit: 500})
	compositionOK(t, err)
	if len(page.Events) != 1 || !page.Events[0].At.Equal(now) {
		t.Fatal("child acceptance was backdated", page)
	}
	compositionOK(t, argojob.CheckChildMessagesForTest())
}

func TestRemovedScheduleChildCanStopAndStillObserve(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	e := f.executor()
	compositionOK(t, e.RecordChildForTest(t.Context(), f.child))
	g := f.config.Git.(*cronfake.Git)
	ref, err := g.ResolveRef(t.Context(), "origin/team/repo", "refs/heads/main")
	compositionOK(t, err)
	g.PutRef("origin/team/repo", gitport.Ref{Name: ref.Name, ObjectSHA: strings.Repeat("e", 40), CommitSHA: strings.Repeat("e", 40)})
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	if len(f.child.OwnerReferences) != 0 {
		t.Fatal("fake did not orphan the native child")
	}
	proof, err := f.s.ChildProof(t.Context(), f.child.Name)
	compositionOK(t, err)
	compositionOK(t, e.Stop(t.Context(), proof.Run))
	_, err = e.Adopt(t.Context(), proof.Run)
	compositionOK(t, err)
	_, err = f.s.ScheduleAdmission(t.Context(), proof.Run.RepositoryID, proof.ScheduleEntry, f.prepared.View().Digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("removed entry retained snapshots", err)
	}
}

func advanceChildSchedule(t *testing.T, f *childFixture, commit, body string) {
	t.Helper()
	g := f.config.Git.(*cronfake.Git)
	sha := strings.Repeat(commit, 40)
	g.PutRef("origin/team/repo", gitport.Ref{Name: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha})
	g.PutFile(gitport.Revision{Repository: "origin/team/repo", Commit: sha}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("d", 40), Bytes: []byte(body)})
	compositionOK(t, f.syncer.SyncAll(t.Context()))
}

func TestChildOlderThanPreviousAdmissionRefusedOnce(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	for _, c := range []string{"b", "c"} {
		advanceChildSchedule(t, f, c, "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	}
	c := f.executorConfig()
	var log bytes.Buffer
	c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	e := argojob.New(c)
	for range 3 {
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	if e.ChildRetriesForTest(f.child.Name) != 0 || strings.Count(log.String(), "level=ERROR") != 1 || !strings.Contains(log.String(), "scheduled admission is no longer retained") {
		t.Fatal("old admission was not permanently reported once", log.String())
	}
	runs, err := f.s.Runs(t.Context(), stateport.RunFilter{Limit: 200})
	compositionOK(t, err)
	if len(runs) != 0 {
		t.Fatal("old admission replay gained a run")
	}
}

func TestChildAnotherEntryAdmissionRefused(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}, {name: other, cron: '0 4 * * *'}]")
	name, err := nativebinding.CronName(1, "other")
	compositionOK(t, err)
	other, err := f.native.Get(t.Context(), name, meta.GetOptions{})
	compositionOK(t, err)
	f.child.Annotations[nativebinding.AdmissionAnnotation] = other.Spec.WorkflowMetadata.Annotations[nativebinding.AdmissionAnnotation]
	err = f.executor().RecordChildForTest(t.Context(), f.child)
	if err == nil || !publicerr.Permanent(err) {
		t.Fatal("other entry admission became a retry", err)
	}
}

func TestStoppedChildRecordedOnceAsStopped(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	f.child.Spec.Shutdown = wf.ShutdownStrategyTerminate
	f.child.Status.Phase = wf.WorkflowFailed
	c := f.executorConfig()
	reports := 0
	c.Reporter = &reportSpy{FinishedFunc: func(ctx context.Context, id string, o argojob.Observation) error {
		reports++
		r, err := f.s.Run(ctx, id)
		if err != nil {
			return err
		}
		return f.s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: id, Revision: r.Revision, Status: o.Status, Reason: o.Reason})
	}}
	e := argojob.New(c)
	for range 5 {
		e.ProcessForTest(t.Context(), f.child.Name)
	}
	r, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	if reports != 1 || r.Status != "cancelled" || r.DisplayReason != "stopped" {
		t.Fatal("stopped child was not recorded once", reports, r)
	}
}

func TestTerminalChildIgnoredRegardlessOfEvidence(t *testing.T) {
	t.Parallel()
	for _, status := range []stepmodel.RunStatus{stepmodel.RunPassed, stepmodel.RunFailed, stepmodel.RunCancelled} {
		t.Run(string(status), func(t *testing.T) {
			f := newChildFixture(t)
			compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
			r, err := f.s.Run(t.Context(), f.child.Name)
			compositionOK(t, err)
			compositionOK(t, f.s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: r.ID, Revision: r.Revision, Status: status}))
			c := f.executorConfig()
			c.Reporter = &reportSpy{FinishedFunc: func(context.Context, string, argojob.Observation) error {
				t.Error("terminal child reported again")
				return nil
			}}
			e := argojob.New(c)
			for range 3 {
				e.ProcessForTest(t.Context(), f.child.Name)
			}
			if e.ChildRetriesForTest(f.child.Name) != 0 {
				t.Fatal("terminal child requeued")
			}
		})
	}
}

func TestScheduleSyncPrunesAdmissionWithoutNativeParent(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	v := f.prepared.View()
	v.ScheduleEntry = "unused"
	p, err := admissioncontract.Seal(v)
	compositionOK(t, err)
	compositionOK(t, f.s.RetainSchedule(t.Context(), p))
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "unused", p.View().Digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("unused admission survived complete declaration sync", err)
	}
}

func TestMalformedChildAdmissionIsReportedOnce(t *testing.T) {
	t.Parallel()
	for name, digest := range map[string]string{"empty": "", "uppercase": strings.Repeat("A", 64), "short": strings.Repeat("f", 40), "nonhex": "not-a-digest"} {
		t.Run(name, func(t *testing.T) {
			f := newChildFixture(t)
			f.child.Annotations[nativebinding.AdmissionAnnotation] = digest
			c := f.executorConfig()
			var log bytes.Buffer
			c.Logger = slog.New(slog.NewTextHandler(&log, nil))
			e := argojob.New(c)
			for range 3 {
				e.ProcessForTest(t.Context(), f.child.Name)
			}
			if e.ChildRetriesForTest(f.child.Name) != 0 || strings.Count(log.String(), "level=ERROR") != 1 || !strings.Contains(log.String(), "scheduled admission annotation is invalid") {
				t.Fatal("malformed admission was not refused with its own reason", log.String())
			}
		})
	}
}

func TestSchedulePrunesRemovedAdmissionsDuringApplyOutage(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	v := f.prepared.View()
	v.ScheduleEntry = "unused"
	p, err := admissioncontract.Seal(v)
	compositionOK(t, err)
	compositionOK(t, f.s.RetainSchedule(t.Context(), p))
	config := f.config
	outage := errors.New("admission unavailable")
	config.Admit = func(context.Context, cronsync.Preparation) (*admissioncontract.PreparedAdmission, error) {
		return nil, outage
	}
	syncer, err := cronsync.New(config)
	compositionOK(t, err)
	if err = syncer.SyncAll(t.Context()); !errors.Is(err, outage) {
		t.Fatal("apply outage not surfaced", err)
	}
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "unused", p.View().Digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("removed entry snapshot survived outage", err)
	}
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", f.prepared.View().Digest)
	compositionOK(t, err)
}
