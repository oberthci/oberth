package store

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Upstream-scoped uplinks (#073): the scope is stored with the uplink, comes
// back with every lookup that authenticates it, and narrows the list queries.

func scopeTestStore(t *testing.T) (*Store, model.Upstream, model.Upstream) {
	t.Helper()
	now := time.Now().UTC()
	s := testStore(t, &now)
	ctx := context.Background()
	github, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci"})
	if err != nil {
		t.Fatal(err)
	}
	codeberg, err := s.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/cloudtaser"})
	if err != nil {
		t.Fatal(err)
	}
	return s, github, codeberg
}

func TestResolveUplinkScopesAcceptsOnlyUpstreamNames(t *testing.T) {
	s, _, _ := scopeTestStore(t)
	ctx := context.Background()
	scopes, err := s.ResolveUplinkScopes(ctx, []string{"github", " codeberg", "github"})
	if err != nil || !slices.Equal(scopes, []string{"codeberg", "github"}) {
		t.Fatalf("scopes = %v, %v", scopes, err)
	}
	for _, input := range []string{"gitlab", "github/oberthci", "github/oberthci/oberth", ""} {
		if _, err := s.ResolveUplinkScopes(ctx, []string{input}); !errors.Is(err, ErrInvalid) {
			t.Errorf("scope %q = %v, want invalid", input, err)
		} else if !strings.Contains(err.Error(), "codeberg, github") {
			t.Errorf("scope %q error does not list the upstreams: %v", input, err)
		}
	}
}

func TestUplinkScopeTravelsWithTheUplink(t *testing.T) {
	s, _, _ := scopeTestStore(t)
	ctx := context.Background()
	digest := make([]byte, 32)
	digest[0] = 7
	credential, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "laptop", Digest: digest})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterUplink(ctx, "admin", model.UplinkSpec{Fingerprint: "SHA256:laptop", Identity: "laptop@work",
		TokenCredentialID: credential.ID, Scope: []string{"github"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActivateTokenCredential(ctx, credential.ID); err != nil {
		t.Fatal(err)
	}
	byKey, err := s.UplinkByFingerprint(ctx, "SHA256:laptop")
	if err != nil || !slices.Equal(byKey.Scope, []string{"github"}) {
		t.Fatalf("by fingerprint = %#v, %v", byKey, err)
	}
	byToken, err := s.AuthenticatedUplinkByDigest(ctx, digest)
	if err != nil || !slices.Equal(byToken.Scope, []string{"github"}) {
		t.Fatalf("by token = %#v, %v", byToken, err)
	}
	listed, err := s.ListUplinkStatuses(ctx)
	if err != nil || len(listed) != 1 || !slices.Equal(listed[0].Scope, []string{"github"}) {
		t.Fatalf("listed = %#v, %v", listed, err)
	}
	if action, err := s.LatestAuditActionByType(ctx, "uplink.register", "uplink"); err != nil ||
		!strings.Contains(action.Details, `"scope":["github"]`) || !strings.Contains(action.Details, `"previous_scope":[]`) {
		t.Fatalf("registration audit = %#v, %v", action, err)
	}

	// Re-registering the key without a scope (onboarding, a manual re-add)
	// keeps the scope; only SetUplinkScope widens it.
	digest2 := make([]byte, 32)
	digest2[0] = 8
	second, err := s.CreateTokenCredential(ctx, model.TokenCredentialSpec{Name: "laptop", Digest: digest2})
	if err != nil {
		t.Fatal(err)
	}
	readded, err := s.RegisterUplink(ctx, "admin", model.UplinkSpec{Fingerprint: "SHA256:laptop", Identity: "laptop@work", TokenCredentialID: second.ID})
	if err != nil || !slices.Equal(readded.Scope, []string{"github"}) {
		t.Fatalf("re-registered = %#v, %v", readded, err)
	}
	if again, _ := s.UplinkByFingerprint(ctx, "SHA256:laptop"); !slices.Equal(again.Scope, []string{"github"}) {
		t.Fatalf("re-registration widened the scope to %v", again.Scope)
	}
	if action, err := s.LatestAuditActionByType(ctx, "uplink.register", "uplink"); err != nil ||
		!strings.Contains(action.Details, `"scope":["github"]`) || !strings.Contains(action.Details, `"previous_scope":["github"]`) {
		t.Fatalf("re-registration audit = %#v, %v", action, err)
	}

	// Widen, then clear; each change is audited with the previous scope.
	changed, err := s.SetUplinkScope(ctx, "admin", "laptop@work", "", []string{"github", "codeberg"})
	if err != nil || !slices.Equal(changed.Scope, []string{"codeberg", "github"}) {
		t.Fatalf("set scope = %#v, %v", changed, err)
	}
	action, err := s.LatestAuditActionByType(ctx, "uplink.scope", "uplink")
	var parsed map[string]any
	if err != nil || json.Unmarshal([]byte(action.Details), &parsed) != nil ||
		len(parsed["scope"].([]any)) != 2 || len(parsed["previous_scope"].([]any)) != 1 {
		t.Fatalf("scope audit = %#v, %v", action, err)
	}
	if cleared, err := s.SetUplinkScope(ctx, "admin", "laptop@work", "", nil); err != nil || len(cleared.Scope) != 0 {
		t.Fatalf("clear = %#v, %v", cleared, err)
	}
	if again, _ := s.UplinkByFingerprint(ctx, "SHA256:laptop"); len(again.Scope) != 0 {
		t.Fatalf("cleared uplink still scoped: %v", again.Scope)
	}
	if _, err := s.SetUplinkScope(ctx, "admin", "nobody@work", "", []string{"github"}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown uplink = %v", err)
	}
	if _, err := s.SetUplinkScope(ctx, "admin", "laptop@work", "", []string{"github/oberthci/oberth"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("repository scope = %v, want invalid", err)
	}

	// Removing the uplink removes its scope rows.
	if _, err := s.SetUplinkScope(ctx, "admin", "laptop@work", "", []string{"github"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveUplink(ctx, "admin", "laptop@work", ""); err != nil {
		t.Fatal(err)
	}
	var rows int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM uplink_scopes`).Scan(&rows); err != nil || rows != 0 {
		t.Fatalf("scope rows after removal = %d, %v", rows, err)
	}
}

func TestListsNarrowToScopedRepositoriesAndUpstreams(t *testing.T) {
	s, github, codeberg := scopeTestStore(t)
	ctx := context.Background()
	visible, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: github.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.CreateRepository(ctx, model.RepositorySpec{Name: "secret", UpstreamID: codeberg.ID, DefaultBranch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, repo := range []model.Repository{visible, hidden, hidden} {
		run, err := s.EnqueueRun(ctx, testRunSpec(repo.ID, "feature/"+repo.Name, strings.Repeat("a", 40)))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.UpsertCIIssue(ctx, "agent", repo.ID, "feature/"+repo.Name+run.ID[:4], "red", "b"); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AppendPromotion(ctx, model.PromotionSpec{RepoID: repo.ID, SourceBranch: "f", SourceSHA: strings.Repeat("b", 40), TargetRef: "main", Actor: "agent"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, upstream := range []int64{github.ID, codeberg.ID, 0} {
		if _, err := s.CreateManualIssue(ctx, "agent", model.ManualIssueSpec{Title: "manual", Body: "b", ScopeUpstreamID: upstream}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.CreateManualIssue(ctx, "agent", model.ManualIssueSpec{Title: "x", ScopeUpstreamID: 999}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("issue under a missing upstream = %v", err)
	}

	runs, err := s.ListRecentRuns(ctx, model.RunListFilter{RepoIDs: []int64{visible.ID}})
	if err != nil || len(runs) != 1 || runs[0].RepoID != visible.ID {
		t.Fatalf("scoped runs = %#v, %v", runs, err)
	}
	if none, _ := s.ListRecentRuns(ctx, model.RunListFilter{RepoIDs: []int64{}}); len(none) != 0 {
		t.Fatalf("empty restriction listed %d runs", len(none))
	}
	if all, _ := s.ListRecentRuns(ctx, model.RunListFilter{}); len(all) != 3 {
		t.Fatalf("unrestricted runs = %d", len(all))
	}
	promotions, err := s.ListPromotions(ctx, model.PromotionListFilter{RepoIDs: []int64{visible.ID}})
	if err != nil || len(promotions.Promotions) != 1 {
		t.Fatalf("scoped promotions = %#v, %v", promotions, err)
	}
	issues, err := s.ListIssues(ctx, model.IssueListFilter{UpstreamIDs: []int64{github.ID}})
	if err != nil || len(issues.Issues) != 2 || issues.Total != 2 || issues.Counts.Manual+issues.Counts.CI != 2 {
		t.Fatalf("scoped issues = %#v, %v", issues, err)
	}
	for _, issue := range issues.Issues {
		if issue.RepoID != visible.ID && issue.ScopeUpstreamID != github.ID {
			t.Fatalf("out-of-scope issue listed: %#v", issue)
		}
	}
	if paged, _ := s.ListIssues(ctx, model.IssueListFilter{UpstreamIDs: []int64{github.ID}, Limit: 1}); len(paged.Issues) != 1 || paged.NextBefore == 0 || paged.Total != issues.Total || paged.Counts != issues.Counts {
		t.Fatalf("scoped issue page = %#v", paged)
	}
	if all, _ := s.ListIssues(ctx, model.IssueListFilter{}); len(all.Issues) != 6 {
		t.Fatalf("unrestricted issues = %d", len(all.Issues))
	}
	upstreams, err := s.RepositoryUpstreamNames(ctx)
	if err != nil || upstreams[visible.ID] != "github" || upstreams[hidden.ID] != "codeberg" {
		t.Fatalf("repository upstreams = %v, %v", upstreams, err)
	}
}
