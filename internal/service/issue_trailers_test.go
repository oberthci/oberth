package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

// This exercises the existing finalization hook, including replay of a
// delivered publication after its notification was lost.
func TestGreenPublicationClosesTrailerIssue(t *testing.T) {
	f := newControlFixture(t)
	ctx := context.Background()
	issue, err := f.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: f.repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	f.scheduler.git = &trailerTestGit{controlGit: f.git, issue: issue.ID, sha: sha}
	run, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: sha, Actor: "owner", Trigger: "push"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(ctx, model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: sha, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.store.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := f.scheduler.finishPublication(ctx, f.repo, final); err != nil {
			t.Fatal(err)
		}
	}
	got, err := f.store.Issue(ctx, issue.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != model.IssueClosed || strings.Count(got.Body, run.ID) != 1 || !strings.Contains(got.Body, "green on main") {
		t.Fatalf("green trailer issue = %#v", got)
	}
}

type trailerTestGit struct {
	*controlGit
	issue int64
	sha   string
}

func (g *trailerTestGit) GreenIssueTrailers(context.Context, string, string, string) (bool, []model.CommitIssueTrailer, error) {
	return true, []model.CommitIssueTrailer{{IssueID: g.issue, SHA: g.sha, Close: true}}, nil
}

func TestGreenFastForwardTrailerUsesSourceRun(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	seedGreenPromotionCandidate(t, f, sha)
	issue, err := f.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: f.repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler.git = &trailerTestGit{controlGit: f.git, issue: issue.ID, sha: sha}
	promotion, err := f.store.AppendPromotion(ctx, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/promote", SourceSHA: sha, TargetRef: "main", Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.PlanPromotion(ctx, promotion.ID, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", sha); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(ctx, model.PublicationSpec{RepoID: f.repo.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PreviousKnown: true, ResultSHA: sha, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, ""); err != nil {
		t.Fatal(err)
	}
	// Replay through terminal delivery, as happens after a lost finalization reply.
	if _, err = f.scheduler.DeliverPublication(ctx, pub.ID); err != nil {
		t.Fatal(err)
	}
	source, err := f.store.ResolveRun(ctx, f.repo.ID, sha)
	if err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueClosed || !strings.Contains(got.Body, source.ID) {
		t.Fatalf("fast-forward=%#v %v", got, err)
	}
}

func TestGreenTrailerRecoveryRetainsFailedWork(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	issue, err := f.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: f.repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	g := &trailerFailureGit{trailerTestGit: trailerTestGit{controlGit: f.git, issue: issue.ID, sha: sha}, fail: true}
	f.scheduler.git = g
	run, err := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: sha, Actor: "owner", Trigger: "branch"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(ctx, model.PublicationSpec{RepoID: f.repo.ID, RunID: run.ID, RefKind: run.RefKind, Ref: run.Ref, ResultSHA: sha, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.store.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.finishPublication(ctx, f.repo, final); err != nil {
		t.Fatal("optional policy stopped publication", err)
	}
	pending, err := f.store.PendingIssueTrailerWork(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v %v", pending, err)
	}
	// Startup recovery must retry a delivered publication even with no pending outbox.
	g.fail = false
	if err = f.scheduler.recoverPendingPublicationsWithGate(ctx, f.scheduler.requireMutation); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueClosed {
		t.Fatalf("recovery=%#v %v", got, err)
	}
}

type trailerFailureGit struct {
	trailerTestGit
	fail bool
}

func (g *trailerFailureGit) GreenIssueTrailers(ctx context.Context, repo, previous, head string) (bool, []model.CommitIssueTrailer, error) {
	if g.fail {
		return false, nil, fmt.Errorf("invalid repository policy")
	}
	return g.trailerTestGit.GreenIssueTrailers(ctx, repo, previous, head)
}

func TestGreenPromotionTrainClosesTrailerIssue(t *testing.T) {
	f := newControlFixture(t)
	ctx := t.Context()
	sourceA, sourceB, merged, previous := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40), strings.Repeat("d", 40)
	seedGreenPromotionCandidate(t, f, sourceA)
	seedGreenPromotionCandidate(t, f, sourceB)
	issue, err := f.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: f.repo.UpstreamID})
	if err != nil {
		t.Fatal(err)
	}
	f.scheduler.git = &trailerTestGit{controlGit: f.git, issue: issue.ID, sha: merged}
	train := &model.PromotionTrain{Sources: []model.PromotionSource{{SHA: sourceA, Ref: "feature/promote"}, {SHA: sourceB, Ref: "feature/promote"}}}
	promotion, err := f.store.AppendPromotion(ctx, model.PromotionSpec{RepoID: f.repo.ID, SourceBranch: "feature/promote", SourceSHA: sourceA, TargetRef: "main", Actor: "owner", Train: train})
	if err != nil {
		t.Fatal(err)
	}
	train.TreeSHA = strings.Repeat("e", 40)
	if _, err = f.store.PlanPromotionTrain(ctx, promotion.ID, previous, merged, train); err != nil {
		t.Fatal(err)
	}
	queued, _, err := f.store.EnqueueAdmittedPromotionRun(ctx, model.RunSpec{RepoID: f.repo.ID, RefKind: model.RefBranch, Ref: promotionRunRef("main", merged), SHA: merged, TestedSHA: merged, BaseSHA: previous, Actor: "owner", Trigger: "promotion"}, promotion.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.ClaimNextRun(ctx); err != nil {
		t.Fatal(err)
	}
	pub, err := f.store.BeginPublication(ctx, model.PublicationSpec{RepoID: f.repo.ID, RunID: queued.ID, PromotionID: promotion.ID, RefKind: model.RefBranch, Ref: "main", PreviousSHA: previous, PreviousKnown: true, ResultSHA: merged, Actor: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	final, err := f.store.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.scheduler.finishPublication(ctx, f.repo, final); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.Issue(ctx, issue.ID)
	if err != nil || got.State != model.IssueClosed || !strings.Contains(got.Body, queued.ID) || !strings.Contains(got.Body, merged[:12]) {
		t.Fatalf("train trailer=%#v %v", got, err)
	}
}

func TestGreenTrailerPoisonedRepositoryDoesNotStarveOthers(t *testing.T) {
	for _, permanent := range []bool{false, true} {
		t.Run(fmt.Sprintf("permanent=%v", permanent), func(t *testing.T) {
			f := newControlFixture(t)
			ctx := t.Context()
			healthy, err := f.store.CreateRepository(ctx, model.RepositorySpec{Name: "healthy", UpstreamID: f.repo.UpstreamID, DefaultBranch: "main"})
			if err != nil {
				t.Fatal(err)
			}
			issue, err := f.store.CreateManualIssue(ctx, "owner", model.ManualIssueSpec{Title: "repair", ScopeUpstreamID: healthy.UpstreamID})
			if err != nil {
				t.Fatal(err)
			}
			poison := errors.New("repository is not cached")
			if permanent {
				poison = fmt.Errorf("%w: invalid repository policy", model.ErrIssueTrailerPermanent)
			}
			g := &poisonedTrailerGit{trailerTestGit: trailerTestGit{controlGit: f.git, issue: issue.ID, sha: strings.Repeat("b", 40)}, poison: poison}
			f.scheduler.git = g
			publish := func(repo model.Repository, sha string) {
				t.Helper()
				run, e := f.store.EnqueueRun(ctx, model.RunSpec{RepoID: repo.ID, RefKind: model.RefBranch, Ref: "main", SHA: sha, Actor: "owner", Trigger: "push"})
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.store.ClaimNextRun(ctx); e != nil {
					t.Fatal(e)
				}
				pub, e := f.store.BeginPublication(ctx, model.PublicationSpec{RepoID: repo.ID, RunID: run.ID, RefKind: model.RefBranch, Ref: "main", ResultSHA: sha, Actor: "owner"})
				if e != nil {
					t.Fatal(e)
				}
				if _, e = f.store.FinalizePublication(ctx, pub.ID, model.PublicationDelivered, ""); e != nil {
					t.Fatal(e)
				}
			}
			for i := range 100 {
				publish(f.repo, fmt.Sprintf("%040x", i+1))
			}
			publish(healthy, g.sha)
			// Even a retryable Git outage for all 100 older rows must leave room for
			// another repository in the first recovery batch.
			recoveryErr := f.scheduler.recoverIssueTrailers(ctx, f.scheduler.requireMutation)
			if permanent && recoveryErr != nil {
				t.Fatal(recoveryErr)
			}
			if !permanent && recoveryErr == nil {
				t.Fatal("Git outage must remain retryable")
			}
			got, e := f.store.Issue(ctx, issue.ID)
			if e != nil || got.State != model.IssueClosed {
				t.Fatalf("healthy repo starved: %#v %v", got, e)
			}
			// The second tick drains the last permanently rejected row, or retries
			// the unavailable repository without duplicating the healthy evidence.
			recoveryErr = f.scheduler.recoverIssueTrailers(ctx, f.scheduler.requireMutation)
			if permanent && recoveryErr != nil {
				t.Fatal(recoveryErr)
			}
			pending, e := f.store.PendingIssueTrailerWork(ctx)
			want := 100
			if permanent {
				want = 0
			}
			if e != nil || len(pending) != want {
				t.Fatalf("pending=%d want=%d err=%v", len(pending), want, e)
			}
			actions, _, e := f.store.ListAuditActions(ctx, model.AuditListFilter{ActionPrefix: "issue.trailer.skipped", Limit: 100})
			if e != nil {
				t.Fatal(e)
			}
			want = 0
			if permanent {
				want = 100
			}
			if len(actions) != want {
				t.Fatalf("skip audits=%d want=%d", len(actions), want)
			}
			for _, a := range actions {
				if a.Actor != "oberth" || a.ResourceType != "publication" || !strings.Contains(a.Details, "run_id") || !strings.Contains(a.Details, "invalid repository policy") {
					t.Fatalf("skip audit=%#v", a)
				}
			}
			if _, e = f.store.VerifyAuditChain(ctx); e != nil {
				t.Fatal(e)
			}
		})
	}
}

type poisonedTrailerGit struct {
	trailerTestGit
	poison error
}

func (g *poisonedTrailerGit) GreenIssueTrailers(ctx context.Context, repo, previous, head string) (bool, []model.CommitIssueTrailer, error) {
	if strings.HasSuffix(repo, "/healthy") {
		return g.trailerTestGit.GreenIssueTrailers(ctx, repo, previous, head)
	}
	return false, nil, g.poison
}
