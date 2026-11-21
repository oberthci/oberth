package argojob_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
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
)

func TestSuccessfulApplyKeepsPublishedPredecessorAfterOutage(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	published := f.prepared.View().Digest
	config := f.config
	config.Client = failingUpdate{f.native}
	failed, err := cronsync.New(config)
	compositionOK(t, err)
	f.syncer = failed
	var pending []string
	for _, commit := range []string{"b", "c"} {
		advanceChildRef(f, commit)
		if err := failed.SyncAll(t.Context()); err == nil {
			t.Fatal("apply outage was ignored")
		}
		pending = append(pending, f.prepared.View().Digest)
	}
	f.syncer, err = cronsync.New(f.config)
	compositionOK(t, err)
	advanceChildSchedule(t, f, "d", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	current := f.prepared.View().Digest
	for range 3 {
		compositionOK(t, f.syncer.SyncAll(t.Context()))
	}
	for _, digest := range []string{published, current} {
		_, err := f.s.ScheduleAdmission(t.Context(), 1, "nightly", digest)
		compositionOK(t, err)
	}
	for _, digest := range pending {
		_, err := f.s.ScheduleAdmission(t.Context(), 1, "nightly", digest)
		if publicerr.Classify(err).Code() != publicerr.Forbidden {
			t.Fatal("failed apply admission survived publication", err)
		}
	}
}

func TestRepositoryLeavesCatalogueForgetsAdmissions(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	digest := f.prepared.View().Digest
	config := f.config
	config.Repositories = func(context.Context) ([]cronsync.Repository, error) { return nil, nil }
	syncer, err := cronsync.New(config)
	compositionOK(t, err)
	compositionOK(t, syncer.SyncAll(t.Context()))
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", digest)
	if publicerr.Classify(err).Code() != publicerr.NotFound {
		t.Fatal("removed repository retained its admission", err)
	}
}

func TestRestoredChildCurrentAdmissionRetriesUntilVisible(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	older := f.prepared
	advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	var err error
	f.parent, err = f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	f.child = f.fire(t)
	f.s = compositionStore(t)
	registerCompositionRepository(t, f.s, "origin/team/repo")
	compositionOK(t, f.s.RetainSchedule(t.Context(), older))
	compositionOK(t, f.s.PublishSchedule(t.Context(), 1, "nightly", []string{older.View().Digest, ""}))
	config := f.config
	config.RetainAdmission, config.PublishAdmission = f.s.RetainSchedule, f.s.PublishSchedule
	config.ForgetAdmission, config.PruneAdmissions = f.s.ForgetSchedule, f.s.PruneSchedules
	f.syncer, err = cronsync.New(config)
	compositionOK(t, err)
	c := f.executorConfig()
	var log bytes.Buffer
	c.Logger = slog.New(slog.NewTextHandler(&log, nil))
	e := argojob.New(c)
	e.ProcessForTest(t.Context(), f.child.Name)
	if e.ChildRetriesForTest(f.child.Name) != 1 || !strings.Contains(log.String(), "scheduled admission not found") {
		t.Fatal("restored live admission was permanently refused", log.String())
	}
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	e.ProcessForTest(t.Context(), f.child.Name)
	if e.ChildRetriesForTest(f.child.Name) != 0 {
		t.Fatal("visible admission did not settle retry")
	}
	_, err = f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
}

type readingChildCache struct {
	*nativefake.Cache
	reads int
}

func (c *readingChildCache) Workflow(name string) (*wf.Workflow, bool) {
	c.reads++
	return c.Cache.Workflow(name)
}

func TestChildChangeReadsCacheOnlyForNotices(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	c := f.executorConfig()
	cache := &readingChildCache{Cache: c.Cache.(*nativefake.Cache)}
	c.Cache = cache
	e := argojob.New(c)
	cache.Changed(f.child.Name)
	if cache.reads != 0 {
		t.Fatal("ordinary event copied the Workflow", cache.reads)
	}
	f.child.OwnerReferences[0].UID = "foreign"
	e.ProcessForTest(t.Context(), f.child.Name)
	cache.reads = 0
	cache.Changed(f.child.Name)
	if cache.reads != 1 || e.ChildNoticesForTest() != 1 {
		t.Fatal("notice membership was not checked")
	}
	f.child.UID = "replacement"
	cache.Changed(f.child.Name)
	if e.ChildNoticesForTest() != 0 {
		t.Fatal("changed UID retained the old refusal")
	}
	cache.reads = 0
	cache.Changed(f.child.Name)
	if cache.reads != 0 {
		t.Fatal("cleared notice still copied the Workflow")
	}
}

type finishedChildRecovery struct {
	*store.Store
	row   stateport.Run
	steps int
}

func (s *finishedChildRecovery) Runs(ctx context.Context, q stateport.RunFilter) ([]stateport.Run, error) {
	if q.Status != stepmodel.RunQueued || q.Cursor != "" {
		return nil, nil
	}
	err := s.Finish(ctx, stateport.Action{}, stateport.RunUpdate{ID: s.row.ID, Revision: s.row.Revision, Status: stepmodel.RunPassed})
	return []stateport.Run{s.row}, err
}
func (s *finishedChildRecovery) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	s.steps++
	return s.Store.Steps(ctx, id)
}
func TestTerminalChildFinishesBetweenRecoveryListAndRead(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	compositionOK(t, f.executor().RecordChildForTest(t.Context(), f.child))
	row, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	s := &finishedChildRecovery{Store: f.s, row: row}
	c := f.executorConfig()
	c.Runs = s
	e := argojob.New(c)
	compositionOK(t, e.RecoverChildrenForTest(t.Context()))
	if s.steps != 0 {
		t.Fatal("finished child restored from stale recovery listing", s.steps)
	}
}

type hiddenChildProof struct{ argojob.ChildRuns }

func (s hiddenChildProof) ChildProof(context.Context, string) (nativebinding.ChildBinding, error) {
	return nativebinding.ChildBinding{}, publicerr.New(publicerr.NotFound, "proof not visible", "Retry observation.", nil)
}
func TestLostAcceptanceReplyThenFinishedChildIsNotRestored(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	lost := f.executorConfig()
	lost.ChildRuns = interruptedChildStore{f.s, true}
	if err := argojob.New(lost).RecordChildForTest(t.Context(), f.child); err == nil {
		t.Fatal("lost acceptance reply was ignored")
	}
	row, err := f.s.Run(t.Context(), f.child.Name)
	compositionOK(t, err)
	compositionOK(t, f.s.Finish(t.Context(), stateport.Action{}, stateport.RunUpdate{ID: row.ID, Revision: row.Revision, Status: stepmodel.RunPassed}))
	s := &finishedChildRecovery{Store: f.s}
	c := f.executorConfig()
	c.Runs, c.ChildRuns = s, hiddenChildProof{f.s}
	compositionOK(t, argojob.New(c).RecordChildForTest(t.Context(), f.child))
	if s.steps != 0 {
		t.Fatal("terminal acceptance replay restored observation", s.steps)
	}
}

func advanceChildRef(f *childFixture, commit string) {
	g := f.config.Git.(*cronfake.Git)
	sha := strings.Repeat(commit, 40)
	g.PutRef("origin/team/repo", gitport.Ref{Name: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha})
	g.PutFile(gitport.Revision{Repository: "origin/team/repo", Commit: sha}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("d", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
}

func TestNativePredecessorSurvivesPublicationStoreOutage(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	original := f.prepared.View().Digest
	config := f.config
	config.PublishAdmission = func(context.Context, int64, string, []string) error { return errors.New("publication unavailable") }
	var err error
	f.syncer, err = cronsync.New(config)
	compositionOK(t, err)
	advanceChildRef(f, "b")
	if err = f.syncer.SyncAll(t.Context()); err == nil {
		t.Fatal("publication outage was ignored")
	}
	published := f.prepared.View().Digest
	f.syncer, err = cronsync.New(f.config)
	compositionOK(t, err)
	advanceChildSchedule(t, f, "c", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", published)
	compositionOK(t, err)
	_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", original)
	if publicerr.Classify(err).Code() != publicerr.Forbidden {
		t.Fatal("native predecessor was replaced by an older database publication", err)
	}
}

func TestUnchangedPublicationDropsFailedAppliesAndKeepsPredecessor(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	previous := f.prepared.View().Digest
	advanceChildSchedule(t, f, "b", "schedules: [{name: nightly, cron: '0 3 * * *'}]")
	current := f.prepared.View().Digest
	config := f.config
	config.Client = failingUpdate{f.native}
	var err error
	f.syncer, err = cronsync.New(config)
	compositionOK(t, err)
	var pending []string
	for _, commit := range []string{"c", "d"} {
		advanceChildRef(f, commit)
		if err = f.syncer.SyncAll(t.Context()); err == nil {
			t.Fatal("apply outage was ignored")
		}
		pending = append(pending, f.prepared.View().Digest)
	}
	f.syncer, err = cronsync.New(f.config)
	compositionOK(t, err)
	advanceChildRef(f, "b")
	compositionOK(t, f.syncer.SyncAll(t.Context()))
	for _, digest := range []string{current, previous} {
		_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", digest)
		compositionOK(t, err)
	}
	for _, digest := range pending {
		_, err = f.s.ScheduleAdmission(t.Context(), 1, "nightly", digest)
		if publicerr.Classify(err).Code() != publicerr.Forbidden {
			t.Fatal("unchanged apply retained an unpublished snapshot", err)
		}
	}
}
