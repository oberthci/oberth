package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

type parkPushGit struct {
	*stubPushGit
	mirrored  []string
	policy    string
	mirrorErr error
	policyErr error
}

func (g *parkPushGit) SyncBranch(_ context.Context, repo, branch, sha string) error {
	g.mirrored = append(g.mirrored, repo+":"+branch+":"+sha)
	return g.mirrorErr
}

func (g *parkPushGit) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return strings.Repeat("b", 40), []byte(g.policy), g.policy != "", g.policyErr
}

func TestParkPushMirrorsWithoutEnqueue(t *testing.T) {
	sha := strings.Repeat("a", 40)
	g := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}}
	q := &stubCIQueue{}
	receipts := &stubReceiveRecorder{}
	p, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: "main"}}, Discoverer: stubRepositoryDiscoverer{}, Git: g, Receives: receipts, Branches: q, Auditor: &deletionAuditor{}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := p.Branch(t.Context(), BranchPush{EventID: "park", Repository: "repo", Branch: "park/review", NewOID: sha, Actor: "agent"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Run != nil || len(q.requests) != 0 || len(g.mirrored) != 1 || len(receipts.events) != 1 {
		t.Fatalf("result=%+v queued=%d mirrored=%v receipts=%v", result, len(q.requests), g.mirrored, receipts.events)
	}
}

type parkDeliveryGit struct {
	DeliveryGit
	policy      string
	ref         string
	policyErr   error
	absentRepo  string
	policyReads atomic.Int64
}

func (g *parkDeliveryGit) ReadDefaultBlob(_ context.Context, repo string, _ string, _ int) (string, []byte, bool, error) {
	g.policyReads.Add(1)
	if g.absentRepo != "" && strings.HasSuffix(repo, "/"+g.absentRepo) {
		return "", nil, false, gitcache.ErrPolicyRepositoryNotCached
	}
	return strings.Repeat("b", 40), []byte(g.policy), g.policy != "", g.policyErr
}
func (g *parkDeliveryGit) ParkSource(context.Context, string, string, string) (string, error) {
	return g.ref, nil
}

func TestAdminPromotesParkWithoutBranchRun(t *testing.T) {
	f := newControlFixture(t)
	f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: strings.Repeat("a", 40), FastForward: true}
	s := f.api(t)
	s.git = &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}
	sha := strings.Repeat("a", 40)
	p, err := s.promote(t.Context(), api.Actor{Identity: "admin", Admin: true}, f.repo.Name, sha, "main")
	if err != nil {
		t.Fatal(err)
	}
	if p.RunID == "" || p.Status != model.PromotionPending {
		t.Fatalf("park promotion must run CI: %+v", p)
	}
}

func TestParkPromotionAuthorizationAndPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, policy, ref  string
		admin, grant, want bool
		policyErr          error
	}{
		{name: "admin", ref: "park/review", admin: true, want: true},
		{name: "ordinary refused", ref: "park/review"},
		{name: "trusted opt in", ref: "park/review", policy: "allow_without_branch_ci: true\n", want: true},
		{name: "exact grant", ref: "park/review", grant: true, want: true},
		{name: "opt in does not authorize ordinary branch", ref: "feature/review", policy: "allow_without_branch_ci: true\n"},
		{name: "protected target refuses admin", ref: "park/review", admin: true, policy: "require_branch_ci: [main]\n"},
		{name: "protected target refuses grant", ref: "park/review", grant: true, policy: "require_branch_ci: [main]\n"},
		{name: "malformed fails closed", ref: "park/review", admin: true, policy: "allow_without_branch_ci: yes please\n"},
		{name: "unknown key fails closed", ref: "park/review", admin: true, policy: "allow_without_branch_ci_typo: true\n"},
		{name: "upstream error fails closed", ref: "park/review", admin: true, policyErr: errors.New("upstream unavailable")},
		{name: "custom prefix", ref: "review/change", admin: true, policy: "park_ref_prefix: refs/heads/review/\n", want: true},
		{name: "prefix boundary", ref: "parked/review", admin: true},
		{name: "missing park ref", admin: true},
		{name: "default branch never parks", ref: "park/review", admin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newControlFixture(t)
			if tc.name == "default branch never parks" {
				_, err := f.store.SetRepositoryDefaultBranch(t.Context(), f.repo.ID, tc.ref)
				if err != nil {
					t.Fatal(err)
				}
			}
			sha := strings.Repeat("a", 40)
			f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: sha, FastForward: true}
			s := f.api(t)
			s.git = &parkDeliveryGit{DeliveryGit: f.git, ref: tc.ref, policy: tc.policy, policyErr: tc.policyErr}
			// The real grant store enforces expiry, exact tuple, reservation and use.
			s.promotionGrants = f.store
			if tc.grant {
				_, err := f.store.AppendPromotionGrant(t.Context(), model.PromotionGrantSpec{RepoID: f.repo.ID, SourceSHA: sha, TargetRef: "main", Issuer: "admin", IssuerFingerprint: "SHA256:admin", ExpiresAt: time.Now().Add(time.Hour)})
				if err != nil {
					t.Fatal(err)
				}
			}
			p, err := s.promote(t.Context(), api.Actor{Identity: "caller", Admin: tc.admin}, f.repo.Name, sha, "main")
			if !tc.want {
				if err == nil {
					t.Fatalf("unexpected authorization: %+v", p)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !p.BranchCISkipped || p.RunID == "" || p.Status != model.PromotionPending {
				t.Fatalf("park promotion: %+v", p)
			}
			run, err := f.store.Run(t.Context(), p.RunID)
			if err != nil {
				t.Fatal(err)
			}
			if run.Reason != "promoted without branch CI" {
				t.Fatalf("reason=%q", run.Reason)
			}
			if len(f.git.promotions) != 0 {
				t.Fatal("untested fast-forward was published")
			}
		})
	}
}

func TestParkPromotionMergedTreeVerdict(t *testing.T) {
	for _, fast := range []bool{true, false} {
		for _, passed := range []bool{true, false} {
			t.Run(fmt.Sprintf("fast=%v/pass=%v", fast, passed), func(t *testing.T) {
				status, step := model.RunFailed, model.StepFailed
				if passed {
					status, step = model.RunPassed, model.StepPassed
				}
				f := newControlFixture(t, JobResult{Status: status, Phase: "test", Steps: []model.StepResult{stepResult(step)}})
				source, merged := strings.Repeat("a", 40), strings.Repeat("c", 40)
				if fast {
					merged = source
				}
				f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: merged, FastForward: fast}
				s := f.api(t)
				s.git = &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}
				p, err := s.promote(t.Context(), api.Actor{Identity: "admin", Admin: true}, f.repo.Name, source, "main")
				if err != nil {
					t.Fatal(err)
				}
				if err = f.scheduler.ProcessNext(t.Context()); err != nil {
					t.Fatal(err)
				}
				p, err = f.store.Promotion(t.Context(), p.ID)
				if err != nil {
					t.Fatal(err)
				}
				want := model.PromotionFailed
				if passed {
					want = model.PromotionPassed
				}
				if p.Status != want || !p.BranchCISkipped {
					t.Fatalf("promotion=%+v", p)
				}
				if len(f.jobs.createdCI) != 1 || f.jobs.createdCI[0].Run.SHA != merged {
					t.Fatalf("did not test merged tree: %+v", f.jobs.createdCI)
				}
				if (len(f.git.promotions) > 0) != passed {
					t.Fatalf("publications=%v passed=%v", f.git.promotions, passed)
				}
			})
		}
	}
}

func TestParkPromotionRetryRechecksPolicy(t *testing.T) {
	f := newControlFixture(t)
	sha := strings.Repeat("a", 40)
	f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: sha, FastForward: true}
	s := f.api(t)
	g := &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review", policy: "allow_without_branch_ci: true\n"}
	s.git = g
	actor := api.Actor{Identity: "reviewer"}
	p, err := s.promote(t.Context(), actor, f.repo.Name, sha, "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.FinishPromotion(t.Context(), p.ID, model.PromotionFailed, p.RunID, "test failed"); err != nil {
		t.Fatal(err)
	}
	g.policy = ""
	if _, err = s.retryPromotion(t.Context(), actor, p, model.PromotionRetryManual); !errors.Is(err, ErrForbidden) {
		t.Fatalf("revoked opt-in retry=%v", err)
	}
	g.policy = "allow_without_branch_ci: true\n"
	retry, err := s.retryPromotion(t.Context(), actor, p, model.PromotionRetryManual)
	if err != nil {
		t.Fatal(err)
	}
	if retry.RunID == "" || retry.RunID == p.RunID || !retry.BranchCISkipped {
		t.Fatalf("retry=%+v", retry)
	}
}

func TestPromotionPolicyParsing(t *testing.T) {
	for _, text := range []string{"park_ref_prefix: refs/tags/park/\n", "park_ref_prefix: refs/heads/\n", "park_ref_prefix: refs/heads/park\n", "allow_without_branch_ci: true\n---\n{}\n", "require_branch_ci: ['[']\n", "require_branch_ci: ['']\n"} {
		if _, err := readPromotionPolicy(t.Context(), &parkDeliveryGit{policy: text}, "repo"); err == nil {
			t.Errorf("accepted %q", text)
		}
	}
}

func TestParkPushPrefixAndRequiredBranchCI(t *testing.T) {
	for _, tc := range []struct {
		name, branch, defaultBranch, policy string
		park                                bool
		mirrorErr                           error
	}{
		{name: "default prefix", branch: "park/change", park: true},
		{name: "ordinary branch", branch: "feature/change"},
		{name: "namespace boundary", branch: "parked/change"},
		{name: "custom prefix", branch: "review/change", policy: "park_ref_prefix: refs/heads/review/\n", park: true},
		{name: "old prefix is ordinary", branch: "park/change", policy: "park_ref_prefix: refs/heads/review/\n"},
		{name: "default branch is ordinary", branch: "park/change", defaultBranch: "park/change"},
		{name: "protected park branch", branch: "park/change", policy: "require_branch_ci: [park/*]\n"},
		{name: "mirror failure is not acknowledged", branch: "park/change", park: true, mirrorErr: errors.New("transport failure")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sha := strings.Repeat("a", 40)
			defaultBranch := tc.defaultBranch
			if defaultBranch == "" {
				defaultBranch = "main"
			}
			git := &parkPushGit{stubPushGit: &stubPushGit{peeled: gitcache.PeeledObject{ObjectSHA: sha, CommitSHA: sha}}, policy: tc.policy, mirrorErr: tc.mirrorErr}
			queue, receipts := &stubCIQueue{}, &stubReceiveRecorder{}
			p, err := NewPushIngestor(PushConfig{Repositories: stubPushRepositories{repository: model.Repository{ID: 1, Name: "repo", DefaultBranch: defaultBranch}}, Discoverer: stubRepositoryDiscoverer{}, Git: git, Receives: receipts, Branches: queue, Auditor: &deletionAuditor{}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := p.Branch(t.Context(), BranchPush{EventID: "push", Repository: "repo", Branch: tc.branch, NewOID: sha, Actor: "agent"})
			if tc.mirrorErr != nil {
				if err == nil || len(receipts.events) != 0 || len(queue.requests) != 0 {
					t.Fatalf("failed mirror result=%+v error=%v receipts=%v", result, err, receipts.events)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if tc.park {
				if result.Run != nil || len(queue.requests) != 0 || len(git.mirrored) != 1 || len(receipts.events) != 1 {
					t.Fatalf("park result=%+v", result)
				}
			} else {
				if result.Run == nil || len(queue.requests) != 1 || len(git.mirrored) != 0 {
					t.Fatalf("ordinary result=%+v", result)
				}
			}
		})
	}
}

func TestParkPromotionPolicyRecheckedAtPublication(t *testing.T) {
	f := newControlFixture(t)
	sha := strings.Repeat("a", 40)
	f.git.plan = gitcache.MergeCandidate{BaseSHA: strings.Repeat("b", 40), MergedSHA: sha, FastForward: true}
	s := f.api(t)
	g := &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}
	s.git = g
	p, err := s.promote(t.Context(), api.Actor{Identity: "admin", Admin: true}, f.repo.Name, sha, "main")
	if err != nil {
		t.Fatal(err)
	}
	// Policy can become stricter while CI is queued or running.
	f.scheduler.git = &parkWorkspaceGit{controlGit: f.git, policy: "require_branch_ci: [main]\n"}
	if err = f.scheduler.requirePublicationTestPolicy(t.Context(), f.repo, model.Publication{PromotionID: p.ID, RunID: p.RunID, Ref: "main", ResultSHA: sha}); err == nil || !strings.Contains(err.Error(), "requires branch CI") {
		t.Fatalf("policy check=%v", err)
	}
	if len(f.git.promotions) != 0 {
		t.Fatal("policy-refused target was published")
	}
}

type parkWorkspaceGit struct {
	*controlGit
	policy string
}

func (g *parkWorkspaceGit) ReadDefaultBlob(context.Context, string, string, int) (string, []byte, bool, error) {
	return strings.Repeat("b", 40), []byte(g.policy), true, nil
}

func TestParkPromotionUsesExistingReasonLine(t *testing.T) {
	for _, status := range []model.RunStatus{model.RunQueued, model.RunRunning, model.RunPassed, model.RunFailed, model.RunInterrupted} {
		run := model.Run{Trigger: "promotion", Status: status, Reason: "promoted without branch CI", Priority: model.PriorityHighest}
		visible := presentationRun(run)
		if !strings.Contains(visible.Reason, "promoted without branch CI") {
			t.Fatalf("status=%s reason=%q", status, visible.DisplayReason)
		}
		if visible.DisplayStatus != model.DisplayRunStatus(run) {
			t.Fatalf("status changed: %+v", visible)
		}
	}
}

func TestParkPromotionUnbornTargetRunsCI(t *testing.T) {
	f := newControlFixture(t, JobResult{Status: model.RunPassed, Phase: "passed", Steps: []model.StepResult{stepResult(model.StepPassed)}})
	sha := strings.Repeat("a", 40)
	f.git.plan = gitcache.MergeCandidate{MergedSHA: sha, FastForward: true, TargetUnborn: true}
	s := f.api(t)
	s.git = &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review"}
	p, err := s.promote(t.Context(), api.Actor{Identity: "admin", Admin: true}, f.repo.Name, sha, "new-target")
	if err != nil {
		t.Fatal(err)
	}
	if p.RunID == "" || p.PreviousSHA != zeroOID {
		t.Fatalf("untested unborn plan=%+v", p)
	}
	if err = f.scheduler.ProcessNext(t.Context()); err != nil {
		t.Fatal(err)
	}
	p, err = f.store.Promotion(t.Context(), p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != model.PromotionPassed || len(f.jobs.createdCI) != 1 || len(f.git.promotions) != 1 {
		t.Fatalf("promotion=%+v jobs=%v pushes=%v", p, f.jobs.createdCI, f.git.promotions)
	}
}

func TestParkPromotionRequiresRepositoryBeforeFetchingPolicy(t *testing.T) {
	f := newControlFixture(t)
	if _, err := f.store.CreateRepository(t.Context(), model.RepositorySpec{Name: "absent", UpstreamID: f.repo.UpstreamID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	s := f.api(t)
	g := &parkDeliveryGit{DeliveryGit: f.git, ref: "park/review", absentRepo: "absent"}
	s.git = g
	if _, _, err := s.resolvePromotionSource(t.Context(), "", strings.Repeat("a", 40)); err == nil || !strings.Contains(err.Error(), "requires repo") || g.policyReads.Load() != 0 {
		t.Fatalf("SHA-only park resolution fetched catalogue policies: reads=%d error=%v", g.policyReads.Load(), err)
	}
	repository, source, err := s.resolvePromotionSource(t.Context(), f.repo.Name, strings.Repeat("a", 40))
	if err != nil {
		t.Fatal(err)
	}
	if repository.ID != f.repo.ID || source.Ref != "park/review" {
		t.Fatalf("source resolved as %+v/%+v", repository, source)
	}
}

func TestGreenBranchEvidenceKeepsScheduledCIAndExcludesPromotionRuns(t *testing.T) {
	sha := strings.Repeat("a", 40)
	for _, trigger := range []string{"", "branch", "schedule", "promotion"} {
		candidate := model.Run{RefKind: model.RefBranch, Status: model.RunPassed, SHA: sha, TestedSHA: sha, Trigger: trigger}
		if got := greenBranchCandidate(candidate); got != (trigger != "promotion") {
			t.Errorf("trigger=%q green branch=%v", trigger, got)
		}
	}
}
