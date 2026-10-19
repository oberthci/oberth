package service

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

type ciIssueSweepStore struct {
	issue      model.Issue
	repo       model.Repository
	upstream   model.Upstream
	mainRun    model.Run
	known      bool
	closedWith string
	closedBy   string
}

func (s *ciIssueSweepStore) ListIssues(_ context.Context, filter model.IssueListFilter) (model.IssuePage, error) {
	if s.issue.State == model.IssueOpen && filter.BeforeID == 0 {
		return model.IssuePage{Issues: []model.Issue{s.issue}}, nil
	}
	return model.IssuePage{}, nil
}
func (s *ciIssueSweepStore) Repository(context.Context, int64) (model.Repository, error) {
	return s.repo, nil
}
func (s *ciIssueSweepStore) Upstream(context.Context, int64) (model.Upstream, error) {
	return s.upstream, nil
}
func (s *ciIssueSweepStore) ResolveRunByRefKind(context.Context, int64, string, model.RefKind) (model.Run, error) {
	return s.mainRun, nil
}
func (s *ciIssueSweepStore) CIBranchWasPublishedOrDeleted(context.Context, int64, string) (bool, error) {
	return s.known, nil
}
func (s *ciIssueSweepStore) AutoCloseCIIssue(_ context.Context, actor string, issue model.Issue, reason string) (bool, error) {
	s.closedBy, s.closedWith = actor, reason
	s.issue.State = model.IssueClosed
	return true, nil
}

type ciIssueSweepGit struct {
	refs        map[string]string
	public      map[string]string
	ancestor    bool
	checks      int
	checkedHead string
	fetches     int
}

func (g *ciIssueSweepGit) Ensure(context.Context, string) (gitcache.Repository, error) {
	panic("CI issue reconciliation must respect pending receive reservations")
}
func (g *ciIssueSweepGit) EnsureForFetch(context.Context, string) (gitcache.Repository, error) {
	g.fetches++
	return gitcache.Repository{DefaultBranch: "main", Stale: true}, nil
}
func (g *ciIssueSweepGit) RemoteRef(_ context.Context, _ string, ref string) (string, bool, error) {
	sha, ok := g.refs[ref]
	return sha, ok, nil
}
func (g *ciIssueSweepGit) RefSHA(_ context.Context, _ string, branch string) (string, error) {
	sha, ok := g.public[branch]
	if !ok {
		return "", errors.New("branch not found in local cache")
	}
	return sha, nil
}
func (g *ciIssueSweepGit) ReleaseReachable(_ context.Context, _, older, _ string) (bool, error) {
	g.checks++
	g.checkedHead = older
	return g.ancestor, nil
}

func TestCIIssueReconcilerClosesMergedAndDeletedBranches(t *testing.T) {
	const branchSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const mainSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		name, branch                                         string
		branchExists, localExists, mainExists, merged, known bool
		publicHead                                           string
		mainStatus                                           model.RunStatus
		wantReason                                           string
		wantChecks                                           int
	}{
		{name: "merged with green main", branch: "feature/red", branchExists: true, localExists: true, mainExists: true, merged: true, publicHead: branchSHA, mainStatus: model.RunPassed, wantReason: "branch merged into main", wantChecks: 1},
		{name: "deleted upstream", branch: "feature/red", known: true, mainStatus: model.RunFailed, wantReason: "branch deleted upstream"},
		{name: "published upstream but live red head", branch: "feature/red", localExists: true, known: true, publicHead: branchSHA, mainStatus: model.RunFailed},
		{name: "still diverged", branch: "feature/red", branchExists: true, localExists: true, mainExists: true, publicHead: branchSHA, mainStatus: model.RunPassed, wantChecks: 1},
		{name: "upstream head merged but accepted red head diverged", branch: "feature/red", branchExists: true, localExists: true, mainExists: true, publicHead: strings.Repeat("c", 40), mainStatus: model.RunPassed, wantChecks: 1},
		{name: "main is red", branch: "feature/red", branchExists: true, localExists: true, mainExists: true, merged: true, publicHead: branchSHA, mainStatus: model.RunFailed},
		{name: "unpublished red branch", branch: "feature/red", localExists: true, publicHead: branchSHA, mainStatus: model.RunPassed},
		{name: "default branch never auto-closes", branch: "main", known: true, mainStatus: model.RunPassed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &ciIssueSweepStore{
				issue:    model.Issue{ID: 42, RepoID: 3, Kind: model.IssueCI, Branch: tc.branch, State: model.IssueOpen},
				repo:     model.Repository{ID: 3, Name: "oberth", UpstreamID: 2, DefaultBranch: "main"},
				upstream: model.Upstream{ID: 2, Name: "github", BaseURL: "ssh://github.com/oberthci"},
				mainRun:  model.Run{ID: "green-main-run", Status: tc.mainStatus},
				known:    tc.known,
			}
			git := &ciIssueSweepGit{refs: map[string]string{}, public: map[string]string{}, ancestor: tc.merged}
			if tc.localExists {
				git.public[tc.branch] = tc.publicHead
			}
			if tc.branchExists {
				git.refs["refs/heads/"+tc.branch] = branchSHA
			}
			if tc.mainExists {
				git.refs["refs/heads/main"] = mainSHA
			}
			gateCalls := 0
			reconciler, err := NewCIIssueReconciler(store, git, func(context.Context) error { gateCalls++; return nil }, log.New(io.Discard, "", 0))
			if err != nil {
				t.Fatal(err)
			}
			if err := reconciler.Tick(t.Context()); err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(store.closedWith, tc.wantReason) || (tc.wantReason == "" && store.closedWith != "") {
				t.Fatalf("close reason = %q, want substring %q", store.closedWith, tc.wantReason)
			}
			if git.checks != tc.wantChecks {
				t.Fatalf("ancestry checks = %d, want %d", git.checks, tc.wantChecks)
			}
			if tc.branch != "main" && git.fetches != 1 {
				t.Fatalf("reservation-safe fetch calls = %d, want 1", git.fetches)
			}
			if tc.wantChecks != 0 && git.checkedHead != tc.publicHead {
				t.Fatalf("checked branch head = %q, want accepted head %q", git.checkedHead, tc.publicHead)
			}
			if tc.wantReason != "" {
				if store.closedBy != ciIssueReconcilerActor || gateCalls != 1 {
					t.Fatalf("close actor = %q, gate calls = %d", store.closedBy, gateCalls)
				}
			} else if gateCalls != 0 {
				t.Fatalf("unclosed issue reached mutation gate %d times", gateCalls)
			}
		})
	}
}
