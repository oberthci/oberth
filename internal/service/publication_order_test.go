package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

type orderedPublicationGit struct {
	*controlGit
	head string
}

func (g *orderedPublicationGit) MirrorBranchMatches(_ context.Context, _, _ string, sha string) (bool, error) {
	return sha == g.head, nil
}

func TestOlderGreenRunPublicationIsAbandonedWithoutCIIssue(t *testing.T) {
	for _, branch := range []string{"main", "feature/ordered"} {
		t.Run(branch, func(t *testing.T) {
			f := newControlFixture(t)
			older, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: branch, SHA: strings.Repeat("a", 40), Actor: "agent", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			newer, err := storetest.EnqueueRun(f.store, t.Context(), model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: branch, SHA: strings.Repeat("b", 40), Actor: "agent", Trigger: "branch"})
			if err != nil {
				t.Fatal(err)
			}
			for range 2 {
				if _, err := f.store.ClaimNextRun(t.Context()); err != nil {
					t.Fatal(err)
				}
			}
			git := &orderedPublicationGit{controlGit: f.git, head: newer.SHA}
			for _, run := range []model.Run{newer.Run, older.Run} {
				p, err := f.store.BeginPublication(t.Context(), model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: run.SHA, Actor: run.Actor})
				if err != nil {
					t.Fatal(err)
				}
				final, err := deliverPublication(t.Context(), f.scheduler.database(), git, f.store, f.repo, f.repo.Name, p)
				if err != nil {
					t.Fatal(err)
				}
				want := model.PublicationDelivered
				if run.ID == older.ID {
					want = model.PublicationAbandoned
				}
				if final.Publication.Status != want || final.Run.Status != model.RunPassed {
					t.Fatalf("out-of-order result=%+v", final)
				}
				if run.ID == older.ID && !strings.Contains(final.Run.Reason, "a newer commit was pushed") {
					t.Fatalf("missing reason: %+v", final.Run)
				}
			}
			if got := f.git.remoteRefs["refs/heads/"+branch]; got != newer.SHA {
				t.Fatalf("upstream=%s want %s", got, newer.SHA)
			}
			if issue, err := f.store.OpenCIIssue(t.Context(), f.repo.ID, branch); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("obsolete green result opened issue: %+v %v", issue, err)
			}
		})
	}
}
