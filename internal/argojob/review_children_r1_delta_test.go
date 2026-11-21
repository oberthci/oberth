package argojob_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	wf "github.com/argoproj/argo-workflows/v4/pkg/apis/workflow/v1alpha1"
	"github.com/oberthci/oberth/internal/cronsync"
	cronfake "github.com/oberthci/oberth/internal/cronsync/fake"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/schedule"
	"github.com/oberthci/oberth/internal/stateport"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type failingUpdate struct{ cronsync.Native }

func (failingUpdate) Update(context.Context, *wf.CronWorkflow, meta.UpdateOptions) (*wf.CronWorkflow, error) {
	return nil, errors.New("native apply outage")
}

// Two admissions that never reach Argo must not evict the one the live parent still fires.
func TestReviewR1FailedAppliesKeepLiveParentAdmission(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	config := f.config
	config.Client = failingUpdate{f.native}
	syncer, err := cronsync.New(config)
	compositionOK(t, err)
	g := f.config.Git.(*cronfake.Git)
	for _, c := range []string{"b", "c"} {
		sha := strings.Repeat(c, 40)
		g.PutRef("origin/team/repo", gitport.Ref{Name: "refs/heads/main", ObjectSHA: sha, CommitSHA: sha})
		g.PutFile(gitport.Revision{Repository: "origin/team/repo", Commit: sha}, gitport.File{Path: schedule.FileName, Mode: 0100644, Blob: strings.Repeat("d", 40), Bytes: []byte("schedules: [{name: nightly, cron: '0 3 * * *'}]")})
		_ = syncer.SyncAll(t.Context()) // apply fails; the live parent keeps the first admission
	}
	live, err := f.native.Get(t.Context(), f.parent.Name, meta.GetOptions{})
	compositionOK(t, err)
	if live.UID != f.parent.UID || live.ResourceVersion != f.parent.ResourceVersion {
		t.Fatal("parent changed despite outage")
	}
	if err := f.executor().RecordChildForTest(t.Context(), f.child); err != nil {
		t.Fatal("child of the live, unchanged parent refused:", err)
	}
}

// A repository whose schedule was synced but never fired has no runs; removing it must not be blocked.
func TestReviewR1RemoveRepositoryWithRetainedSchedule(t *testing.T) {
	t.Parallel()
	f := newChildFixture(t)
	repo, err := f.s.Repository(t.Context(), "origin/team/repo", nil)
	compositionOK(t, err)
	if err := f.s.RemoveRepository(t.Context(), stateport.Action{Actor: "operator"}, repo.ID); err != nil {
		t.Fatal("repository removal blocked by retained schedule admissions:", err)
	}
}
