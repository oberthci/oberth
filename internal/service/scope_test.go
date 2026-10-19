package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/artifacts"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/runlog"
	"github.com/oberthci/oberth/internal/store"
)

// Upstream-scoped uplinks (#073): a work laptop scoped to github must get only
// github content and github counts, and nothing it receives, errors
// included, may name or count what lies on codeberg.

var globalSequence = regexp.MustCompile(`"[A-Za-z_]*[Ss]equence":[1-9]`)

const (
	hiddenRepoName  = "cloudtaser-secret"
	hiddenIssueText = "cloudtaser-confidential"
)

type scopeFixture struct {
	store    *store.Store
	service  *API
	health   *scopedHealthStub
	visible  model.Repository
	hidden   model.Repository
	runs     map[string][]model.Run // by upstream
	issues   map[string][]model.Issue
	global   model.Issue
	promoted map[string]model.Promotion
	grants   map[string]model.PromotionGrant
	github   model.Upstream
	codeberg model.Upstream
}

type scopedHealthStub struct{ scopedFor []string }

func (*scopedHealthStub) Ready(context.Context) error { return nil }

func (*scopedHealthStub) Status(context.Context) (any, error) {
	return map[string]any{"upstreams": 2, "repositories": 2}, nil
}

func (stub *scopedHealthStub) ScopedStatus(_ context.Context, actor api.Actor) (any, error) {
	stub.scopedFor = actor.Scope
	return map[string]any{"upstreams": 1, "repositories": 1}, nil
}

var (
	scopedActor   = api.Actor{Identity: "laptop@work", Fingerprint: "SHA256:laptop", Scope: []string{"github"}}
	scopedAdmin   = api.Actor{Identity: "laptop-admin@work", Fingerprint: "SHA256:admin", Admin: true, Scope: []string{"github"}}
	unscopedActor = api.Actor{Identity: "owner@tuxbox", Fingerprint: "SHA256:owner", Admin: true}
)

func sha(seed string) string { return strings.Repeat(seed, 40)[:40] }

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	fixture := &scopeFixture{store: database, health: &scopedHealthStub{},
		runs: map[string][]model.Run{}, issues: map[string][]model.Issue{},
		promoted: map[string]model.Promotion{}, grants: map[string]model.PromotionGrant{}}
	if fixture.github, err = database.CreateUpstream(ctx, model.UpstreamSpec{Name: "github", Kind: "ssh", BaseURL: "ssh://git@github.com/oberthci"}); err != nil {
		t.Fatal(err)
	}
	if fixture.codeberg, err = database.CreateUpstream(ctx, model.UpstreamSpec{Name: "codeberg", Kind: "ssh", BaseURL: "ssh://git@codeberg.org/cloudtaser"}); err != nil {
		t.Fatal(err)
	}
	if fixture.visible, err = database.CreateRepository(ctx, model.RepositorySpec{Name: "oberth", UpstreamID: fixture.github.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if fixture.hidden, err = database.CreateRepository(ctx, model.RepositorySpec{Name: hiddenRepoName, UpstreamID: fixture.codeberg.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	// More hidden activity than visible, so a global count is never mistaken
	// for a scoped one.
	for upstream, setup := range map[string]struct {
		repo  model.Repository
		runs  int
		seeds string
	}{"github": {fixture.visible, 2, "ab"}, "codeberg": {fixture.hidden, 5, "cd"}} {
		for index := range setup.runs {
			enqueued, err := database.EnqueueRun(ctx, model.RunSpec{RepoID: setup.repo.ID, RefKind: model.RefBranch,
				Ref: fmt.Sprintf("feature/%s-%d", upstream, index), SHA: sha(fmt.Sprintf("%c%d", setup.seeds[0], index)),
				Actor: "owner@tuxbox", Trigger: "push"})
			if err != nil {
				t.Fatal(err)
			}
			run, err := database.Run(ctx, enqueued.ID)
			if err != nil {
				t.Fatal(err)
			}
			fixture.runs[upstream] = append(fixture.runs[upstream], run)
			issue, err := database.UpsertCIIssue(ctx, "owner@tuxbox", setup.repo.ID, run.Ref, "red "+upstream+" "+hiddenIssueText[:0]+run.Ref, "body")
			if err != nil {
				t.Fatal(err)
			}
			fixture.issues[upstream] = append(fixture.issues[upstream], issue)
		}
		manual, err := database.CreateManualIssue(ctx, "owner@tuxbox", model.ManualIssueSpec{
			Title: "manual " + upstream, Body: "filed under " + upstream, ScopeUpstreamID: setup.repo.UpstreamID})
		if err != nil {
			t.Fatal(err)
		}
		fixture.issues[upstream] = append(fixture.issues[upstream], manual)
		promotion, err := database.AppendPromotion(ctx, model.PromotionSpec{RepoID: setup.repo.ID, SourceBranch: "feature/x",
			SourceSHA: fixture.runs[upstream][0].SHA, TargetRef: "main", Actor: "owner@tuxbox"})
		if err != nil {
			t.Fatal(err)
		}
		fixture.promoted[upstream] = promotion
		grant, err := database.AppendPromotionGrant(ctx, model.PromotionGrantSpec{RepoID: setup.repo.ID,
			SourceSHA: fixture.runs[upstream][1].SHA, TargetRef: "main", Issuer: "owner@tuxbox",
			IssuerFingerprint: "SHA256:owner", ExpiresAt: time.Now().Add(time.Hour)})
		if err != nil {
			t.Fatal(err)
		}
		fixture.grants[upstream] = grant
		qualified, err := database.QualifiedRepoName(ctx, setup.repo.ID)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := database.Grant(ctx, qualified, "release", "oberth/data/"+upstream+"/token", "owner@tuxbox"); err != nil {
			t.Fatal(err)
		}
		if err := database.RecordGrantDeclarations(ctx, qualified, setup.repo.Name+"-sha", []store.GrantDeclaration{{Step: "deploy", Path: "oberth/data/" + upstream + "/pending"}}); err != nil {
			t.Fatal(err)
		}
	}
	// A red github run projects a CI issue with a CI work sequence, a global
	// counter that must not reach scoped actors.
	red := fixture.runs["github"][0]
	if _, err := database.FinishRun(ctx, red.ID, model.RunResult{Status: model.RunFailed, Phase: "test", FailedBurn: "test", FailedStep: "unit", Error: "red"}); err != nil {
		t.Fatal(err)
	}
	// The secret upstream's manual issue body names the confidential project.
	if _, err := database.CreateManualIssue(ctx, "owner@tuxbox", model.ManualIssueSpec{
		Title: hiddenIssueText, Body: hiddenIssueText, ScopeUpstreamID: fixture.codeberg.ID}); err != nil {
		t.Fatal(err)
	}
	if fixture.global, err = database.CreateManualIssue(ctx, "owner@tuxbox", model.ManualIssueSpec{Title: "global " + hiddenIssueText, Body: "no upstream"}); err != nil {
		t.Fatal(err)
	}
	logs, err := runlog.Open(filepath.Join(root, "logs"))
	if err != nil {
		t.Fatal(err)
	}
	kept, err := artifacts.Open(filepath.Join(root, "artifacts"))
	if err != nil {
		t.Fatal(err)
	}
	fixture.service, err = NewAPI(APIConfig{
		Runs: database, History: database, Repositories: database, Issues: database,
		Promotions: database, Logs: logs, Artifacts: kept, Auditor: database,
		SecretAccess: database, KubeVirtGrants: database, PromotionGrants: database, RepositoryRemover: database,
		KVMPreflights: database, Health: fixture.health, Signals: NewSignals(), RunCancels: database,
		MaximumWait: 50 * time.Millisecond, PromotionWorkspaceRoot: filepath.Join(root, "promotion-work"),
		SecretStoreVerifier: func(context.Context, api.SecretStoreVerifyRequest) (api.SecretStoreVerifyResponse, error) {
			return api.SecretStoreVerifyResponse{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func (fixture *scopeFixture) call(actor api.Actor, tool string, arguments any) (any, error) {
	raw, err := json.Marshal(arguments)
	if err != nil {
		panic(err)
	}
	return fixture.service.CallTool(context.Background(), actor, tool, raw)
}

// assertNoLeak fails when a scoped response or error names anything on the
// hidden upstream. Echoes of the caller's own input do not count.
func (fixture *scopeFixture) assertNoLeak(t *testing.T, what string, value any, err error, echoes ...string) {
	t.Helper()
	encoded, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		t.Fatalf("%s: marshal: %v", what, marshalErr)
	}
	text := string(encoded)
	// Global sequences count every run and promotion on the installation.
	if sequence := globalSequence.FindString(text); sequence != "" {
		t.Errorf("%s exposes a global sequence %s", what, sequence)
	}
	if err != nil {
		text += " " + err.Error()
	}
	for _, echo := range echoes {
		if echo != "" {
			text = strings.ReplaceAll(text, echo, "<input>")
		}
	}
	needles := []string{"codeberg", "cloudtaser", hiddenRepoName, hiddenIssueText}
	for _, run := range fixture.runs["codeberg"] {
		needles = append(needles, run.ID, run.SHA[:12])
	}
	needles = append(needles, fixture.promoted["codeberg"].ID, fixture.grants["codeberg"].ID)
	for _, needle := range needles {
		if strings.Contains(text, needle) {
			t.Errorf("%s leaks %q: %s", what, needle, text)
		}
	}
}

func TestScopedUplinkSeesOnlyItsUpstream(t *testing.T) {
	fixture := newScopeFixture(t)
	ctx := context.Background()
	visibleRuns, hiddenRuns := fixture.runs["github"], fixture.runs["codeberg"]

	// run_list: only github runs, newest first, and a cursor computed over them.
	value, err := fixture.call(scopedActor, "run_list", map[string]any{})
	fixture.assertNoLeak(t, "run_list", value, err)
	if list := value.(api.RunListResponse); len(list.Runs.([]model.Run)) != len(visibleRuns) || list.NextBefore != 0 {
		t.Fatalf("run_list = %d runs, next %d", len(list.Runs.([]model.Run)), list.NextBefore)
	}
	// A scoped uplink pages by run ID; a sequence cursor, which would let
	// it probe the global run count, is refused.
	value, err = fixture.call(scopedActor, "run_list", map[string]any{"limit": 1})
	fixture.assertNoLeak(t, "paged run_list", value, err)
	first := value.(api.RunListResponse)
	if err != nil || len(first.Runs.([]model.Run)) != 1 || first.NextBefore != 0 || first.NextCursor == "" {
		t.Fatalf("paged run_list = %#v, %v", value, err)
	}
	value, err = fixture.call(scopedActor, "run_list", map[string]any{"limit": 1, "before": first.NextCursor})
	if second := value.(api.RunListResponse); err != nil || len(second.Runs.([]model.Run)) != 1 ||
		second.Runs.([]model.Run)[0].ID == first.Runs.([]model.Run)[0].ID {
		t.Fatalf("second page = %#v, %v", value, err)
	}
	for _, before := range []any{3, "3"} {
		if _, err := fixture.call(scopedActor, "run_list", map[string]any{"before": before}); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("scoped sequence cursor %v = %v, want invalid", before, err)
		}
	}
	if _, err := fixture.service.Runs(ctx, scopedActor, api.RunFilter{Limit: 10, Before: 3}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("scoped /api/runs sequence cursor = %v", err)
	}
	if unscoped := mustCall(t, fixture, unscopedActor, "run_list", map[string]any{"limit": 1}).(api.RunListResponse); unscoped.NextBefore == 0 || unscoped.NextCursor != "" {
		t.Fatalf("unscoped run_list cursor = %#v", unscoped)
	}
	// A long all-digit cursor is tried as a run ID first: the unscoped caller
	// falls back to the sequence, the scoped one gets no sequence fallback.
	if all := mustCall(t, fixture, unscopedActor, "run_list", map[string]any{"before": "999999999999"}).(api.RunListResponse); len(all.Runs.([]model.Run)) != len(visibleRuns)+len(hiddenRuns) {
		t.Fatalf("unscoped long numeric cursor = %#v", all)
	}
	if _, err := fixture.call(scopedActor, "run_list", map[string]any{"before": "999999999999"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("scoped long numeric cursor = %v, want not found", err)
	}

	// Run counts cover github only.
	counts, err := fixture.service.RunCounts(ctx, scopedActor)
	fixture.assertNoLeak(t, "RunCounts", counts, err)
	if got := counts.(model.RunCounts); got.Total != len(visibleRuns) || len(got.ByRepo) != 1 || got.ByRepo[fixture.visible.ID] != len(visibleRuns) {
		t.Fatalf("scoped counts = %#v", got)
	}
	if all, _ := fixture.service.RunCounts(ctx, unscopedActor); all.(model.RunCounts).Total != len(visibleRuns)+len(hiddenRuns) {
		t.Fatalf("unscoped counts = %#v", all)
	}

	// repo_list and the dashboard repository view.
	value, err = fixture.call(scopedActor, "repo_list", map[string]any{})
	fixture.assertNoLeak(t, "repo_list", value, err)
	if repos := value.(api.RepoListResponse).Repositories.([]RepoView); len(repos) != 1 || repos[0].ID != fixture.visible.ID || len(repos[0].LatestRuns) != len(visibleRuns) {
		t.Fatalf("repo_list = %#v", repos)
	}

	// issue_list: github CI issues and the github manual issue, never the
	// codeberg or the workspace-global ones, and paging counts only those.
	wantIssues := len(fixture.issues["github"])
	value, err = fixture.call(scopedActor, "issue_list", map[string]any{})
	fixture.assertNoLeak(t, "issue_list", value, err)
	if page := value.(api.IssueListResponse); len(page.Issues) != wantIssues || page.NextBefore != 0 {
		t.Fatalf("issue_list = %d issues (want %d), next %d", len(page.Issues), wantIssues, page.NextBefore)
	}
	value, err = fixture.call(scopedActor, "issue_list", map[string]any{"limit": wantIssues - 1})
	if page := value.(api.IssueListResponse); err != nil || len(page.Issues) != wantIssues-1 || page.NextBefore == 0 {
		t.Fatalf("paged issue_list = %#v, %v", value, err)
	} else {
		value, err = fixture.call(scopedActor, "issue_list", map[string]any{"before": page.NextBefore})
		if rest := value.(api.IssueListResponse); err != nil || len(rest.Issues) != 1 || rest.NextBefore != 0 {
			t.Fatalf("last issue page = %#v, %v", value, err)
		}
	}
	for _, filter := range []map[string]any{{"kind": "manual"}, {"state": "open"}, {"repo": "github/oberthci/oberth"}} {
		value, err = fixture.call(scopedActor, "issue_list", filter)
		fixture.assertNoLeak(t, fmt.Sprintf("issue_list %v", filter), value, err)
	}
	issues, err := fixture.service.Issues(ctx, scopedActor, api.IssueFilter{})
	fixture.assertNoLeak(t, "Issues view", issues, err)
	if page := issues.(model.IssuePage); len(page.Issues) != wantIssues {
		t.Fatalf("Issues view = %d", len(page.Issues))
	}
	if page, _ := fixture.service.Issues(ctx, unscopedActor, api.IssueFilter{}); len(page.(model.IssuePage).Issues) != wantIssues+len(fixture.issues["codeberg"])+2 {
		t.Fatalf("unscoped issues = %d", len(page.(model.IssuePage).Issues))
	}

	// promotion_list and the grants views.
	value, err = fixture.call(scopedActor, "promotion_list", map[string]any{})
	fixture.assertNoLeak(t, "promotion_list", value, err)
	if list := value.(api.PromotionListResponse); len(list.Promotions) != 1 || list.Promotions[0].ID != fixture.promoted["github"].ID {
		t.Fatalf("promotion_list = %#v", list)
	}
	if _, err := fixture.call(scopedActor, "promotion_list", map[string]any{"before": 5}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("scoped promotion sequence cursor = %v", err)
	}
	if _, err := fixture.call(scopedActor, "promotion_list", map[string]any{"before": fixture.promoted["codeberg"].ID}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("hidden promotion as cursor = %v", err)
	}
	value, err = fixture.call(scopedActor, "promote_grant_list", map[string]any{})
	fixture.assertNoLeak(t, "promote_grant_list", value, err)
	grants, err := fixture.service.Grants(ctx, scopedActor, api.GrantFilter{})
	fixture.assertNoLeak(t, "Grants view", grants, err)
	if list := grants.([]map[string]any); len(list) != 1 {
		t.Fatalf("Grants view = %#v", list)
	}

	// access_list, including pending declarations.
	for _, arguments := range []map[string]any{{}, {"revoked": true}, {"pending": true}} {
		value, err = fixture.call(scopedActor, "access_list", arguments)
		fixture.assertNoLeak(t, fmt.Sprintf("access_list %v", arguments), value, err)
	}
	if list := mustCall(t, fixture, scopedActor, "access_list", map[string]any{}).(api.AccessListResponse); len(list.Grants) != 1 {
		t.Fatalf("access_list = %#v", list)
	}

	// Per-run and per-SHA reads of github work succeed.
	for tool, arguments := range map[string]map[string]any{
		"run_get":        {"id": visibleRuns[0].ID},
		"status":         {"ref": visibleRuns[0].SHA},
		"wait":           {"sha": visibleRuns[0].SHA},
		"artifacts":      {"id": visibleRuns[0].ID},
		"promote_status": {"id": fixture.promoted["github"].ID},
	} {
		value, err := fixture.call(scopedActor, tool, arguments)
		if err != nil {
			t.Fatalf("%s of an in-scope run: %v", tool, err)
		}
		fixture.assertNoLeak(t, tool, value, nil)
	}

	// system_status asks the health for the scoped view.
	value, err = fixture.call(scopedActor, "system_status", map[string]any{})
	if err != nil || !slices.Equal(fixture.health.scopedFor, []string{"github"}) || value.(map[string]any)["upstreams"] != 1 {
		t.Fatalf("system_status = %#v, %v (scoped for %v)", value, err, fixture.health.scopedFor)
	}
}

func mustCall(t *testing.T, fixture *scopeFixture, actor api.Actor, tool string, arguments any) any {
	t.Helper()
	value, err := fixture.call(actor, tool, arguments)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return value
}

// Every lookup of codeberg work answers exactly as the lookup of something
// that does not exist, so a scoped uplink cannot even tell it is there.
func TestScopedUplinkCannotTellHiddenFromMissing(t *testing.T) {
	fixture := newScopeFixture(t)
	hiddenRun := fixture.runs["codeberg"][0]
	missingRunID := strings.Repeat("e", 32)
	hiddenIssue := fixture.issues["codeberg"][0].ID
	missingIssue := int64(99999)
	hiddenRepo := "codeberg/cloudtaser/" + hiddenRepoName
	missingRepo := "codeberg/cloudtaser/does-not-exist"
	missingSHA := sha("9")

	pairs := []struct {
		tool            string
		hidden, missing map[string]any
	}{
		{"run_get", map[string]any{"id": hiddenRun.ID}, map[string]any{"id": missingRunID}},
		{"run_get", map[string]any{"id": hiddenRun.ID[:16]}, map[string]any{"id": missingRunID[:16]}},
		{"run_logs", map[string]any{"id": hiddenRun.ID, "burn": "test", "step": "unit"}, map[string]any{"id": missingRunID, "burn": "test", "step": "unit"}},
		{"artifacts", map[string]any{"id": hiddenRun.ID}, map[string]any{"id": missingRunID}},
		{"artifact_get", map[string]any{"id": hiddenRun.ID, "name": "report"}, map[string]any{"id": missingRunID, "name": "report"}},
		{"run_kvm_preflight", map[string]any{"id": hiddenRun.ID}, map[string]any{"id": missingRunID}},
		{"status", map[string]any{"ref": hiddenRun.SHA}, map[string]any{"ref": missingSHA}},
		{"status", map[string]any{"repo": hiddenRepo, "ref": hiddenRun.SHA}, map[string]any{"repo": missingRepo, "ref": hiddenRun.SHA}},
		{"logs", map[string]any{"sha": hiddenRun.SHA, "step": "unit"}, map[string]any{"sha": missingSHA, "step": "unit"}},
		{"wait", map[string]any{"sha": hiddenRun.SHA}, map[string]any{"sha": missingSHA}},
		{"sync", map[string]any{"sha": hiddenRun.SHA}, map[string]any{"sha": missingSHA}},
		{"promote", map[string]any{"sha": hiddenRun.SHA, "branch": "main"}, map[string]any{"sha": missingSHA, "branch": "main"}},
		{"promote", map[string]any{"repo": hiddenRepo, "sha": hiddenRun.SHA, "branch": "main"}, map[string]any{"repo": missingRepo, "sha": hiddenRun.SHA, "branch": "main"}},
		{"promote_status", map[string]any{"id": fixture.promoted["codeberg"].ID}, map[string]any{"id": "promotion-missing"}},
		{"publish_retry", map[string]any{"id": hiddenRun.ID}, map[string]any{"id": missingRunID}},
		{"run_cancel", map[string]any{"id": hiddenRun.ID, "reason": "x"}, map[string]any{"id": missingRunID, "reason": "x"}},
		{"run_cancel", map[string]any{"id": hiddenRun.ID[:16]}, map[string]any{"id": missingRunID[:16]}},
		{"publish_retry", map[string]any{"id": fixture.promoted["codeberg"].ID}, map[string]any{"id": "promotion-missing"}},
		{"issue_get", map[string]any{"id": hiddenIssue}, map[string]any{"id": missingIssue}},
		{"issue_get", map[string]any{"id": fixture.global.ID}, map[string]any{"id": missingIssue}},
		{"issue_update", map[string]any{"id": hiddenIssue, "title": "t", "body": "b"}, map[string]any{"id": missingIssue, "title": "t", "body": "b"}},
		{"issue_append", map[string]any{"id": hiddenIssue, "text": "x"}, map[string]any{"id": missingIssue, "text": "x"}},
		{"issue_close", map[string]any{"id": hiddenIssue}, map[string]any{"id": missingIssue}},
		{"issue_reopen", map[string]any{"id": fixture.global.ID}, map[string]any{"id": missingIssue}},
		{"issue_delete", map[string]any{"id": fixture.global.ID}, map[string]any{"id": missingIssue}},
		{"issue_lock", map[string]any{"id": hiddenIssue}, map[string]any{"id": missingIssue}},
		{"run_list", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"issue_list", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"promotion_list", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"promote_grant_list", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"access_list", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"kubevirt_grant_get", map[string]any{"repo": hiddenRepo}, map[string]any{"repo": missingRepo}},
		{"access_list", map[string]any{"repo": hiddenRepo, "pending": true}, map[string]any{"repo": missingRepo, "pending": true}},
	}
	for _, pair := range pairs {
		hiddenValue, hiddenErr := fixture.call(scopedActor, pair.tool, pair.hidden)
		missingValue, missingErr := fixture.call(scopedActor, pair.tool, pair.missing)
		fixture.assertNoLeak(t, pair.tool+" hidden", hiddenValue, hiddenErr, hiddenName(pair.hidden), strings.ToLower(hiddenName(pair.hidden)))
		if fmt.Sprint(hiddenErr) != strings.ReplaceAll(fmt.Sprint(missingErr), missingName(pair.missing), hiddenName(pair.hidden)) {
			t.Errorf("%s: hidden error %q, missing error %q", pair.tool, hiddenErr, missingErr)
		}
		if (hiddenErr == nil) != (missingErr == nil) {
			t.Errorf("%s: hidden %v/%#v vs missing %v/%#v", pair.tool, hiddenErr, hiddenValue, missingErr, missingValue)
		}
	}

	// The dashboard's live log reads by run ID outside CallTool.
	_, hiddenErr := fixture.service.RunLogLive(context.Background(), scopedActor, hiddenRun.ID, 0)
	_, missingErr := fixture.service.RunLogLive(context.Background(), scopedActor, missingRunID, 0)
	if hiddenErr == nil || fmt.Sprint(hiddenErr) != fmt.Sprint(missingErr) {
		t.Errorf("livelog: hidden %v, missing %v", hiddenErr, missingErr)
	}

	if run, err := fixture.store.Run(context.Background(), hiddenRun.ID); err != nil || !run.Status.Active() {
		t.Fatalf("scoped run_cancel reached a hidden run: %#v, %v", run, err)
	}
	// The issue that is not hidden still works, and the hidden ones were
	// left untouched.
	if issue, err := fixture.store.Issue(context.Background(), hiddenIssue); err != nil || issue.State != model.IssueOpen {
		t.Fatalf("hidden issue changed: %#v, %v", issue, err)
	}
	value := mustCall(t, fixture, scopedActor, "issue_get_many", map[string]any{"ids": []int64{hiddenIssue, fixture.issues["github"][0].ID, missingIssue}})
	results := value.(api.IssueGetManyResponse).Results
	if results[0].Error != "not_found" || results[1].Issue == nil || results[2].Error != "not_found" {
		t.Fatalf("issue_get_many = %#v", results)
	}
}

// missingName and hiddenName are the selector each half of a pair named, so
// errors that echo the caller's own input compare equal.
func missingName(arguments map[string]any) string { return selectorOf(arguments) }

func hiddenName(arguments map[string]any) string { return selectorOf(arguments) }

func selectorOf(arguments map[string]any) string {
	for _, key := range []string{"repo", "id", "sha", "ref"} {
		if value, ok := arguments[key]; ok {
			return fmt.Sprint(value)
		}
	}
	return ""
}

func TestScopedAdminCannotActOutsideItsUpstream(t *testing.T) {
	fixture := newScopeFixture(t)
	hiddenRepo := "codeberg/cloudtaser/" + hiddenRepoName
	for tool, arguments := range map[string]map[string]any{
		"access_allow":         {"repo": hiddenRepo, "step": "deploy", "secret": "oberth/data/x"},
		"access_revoke":        {"repo": hiddenRepo, "step": "release", "secret": "oberth/data/codeberg/token"},
		"kubevirt_grant":       {"repo": hiddenRepo},
		"kubevirt_revoke":      {"repo": hiddenRepo},
		"repo_remove":          {"repo": hiddenRepo},
		"promote_grant":        {"repo": hiddenRepo, "sha": fixture.runs["codeberg"][0].SHA, "branch": "main", "expires": "1h"},
		"promote_grant_revoke": {"id": fixture.grants["codeberg"].ID},
	} {
		value, err := fixture.call(scopedAdmin, tool, arguments)
		fixture.assertNoLeak(t, tool, value, err, selectorOf(arguments))
		if err == nil || !errors.Is(err, store.ErrNotFound) && !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s outside scope = %#v, %v", tool, value, err)
		}
	}
	for _, tool := range []string{"secretstore_verify", "secretstore_plan", "secretstore_sync_receipt"} {
		if _, err := fixture.call(scopedAdmin, tool, map[string]any{}); !errors.Is(err, ErrForbidden) {
			t.Errorf("%s by a scoped admin = %v, want forbidden", tool, err)
		}
	}
	// Nothing outside the scope changed.
	grants, err := fixture.store.SecretAccessList(context.Background(), "", true)
	if err != nil || len(grants) != 2 {
		t.Fatalf("secret access after refusals = %#v, %v", grants, err)
	}
	if _, err := fixture.store.RepositoryByName(context.Background(), hiddenRepo); err != nil {
		t.Fatalf("hidden repository removed: %v", err)
	}
	// Inside the scope the admin still acts.
	if _, err := fixture.call(scopedAdmin, "access_allow", map[string]any{"repo": "github/oberthci/oberth", "step": "deploy", "secret": "oberth/data/github/deploy"}); err != nil {
		t.Fatalf("in-scope access_allow: %v", err)
	}
	if _, err := fixture.call(scopedAdmin, "kubevirt_grant", map[string]any{"repo": "github/oberthci/oberth"}); err != nil {
		t.Fatalf("in-scope kubevirt_grant: %v", err)
	}
}

func TestScopedIssueCreateFilesUnderTheUpstream(t *testing.T) {
	fixture := newScopeFixture(t)
	ctx := context.Background()
	created := mustCall(t, fixture, scopedActor, "issue_create", map[string]any{"title": "laptop note", "body": "b"}).(api.IssueCreateResponse)
	issue, err := fixture.store.Issue(ctx, created.ID)
	if err != nil || issue.ScopeUpstreamID != fixture.github.ID {
		t.Fatalf("scoped issue = %#v, %v", issue, err)
	}
	if _, err := fixture.call(scopedActor, "issue_create", map[string]any{"title": "x", "body": "b", "upstream": "codeberg"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("issue under a hidden upstream = %v", err)
	}
	if _, err := fixture.call(scopedActor, "issue_create", map[string]any{"title": "x", "body": "b", "upstream": "gitlab"}); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("issue under a missing upstream = %v", err)
	}
	// Several upstreams in scope: the upstream must be named.
	wide := api.Actor{Identity: "wide@work", Fingerprint: "SHA256:wide", Scope: []string{"codeberg", "github"}}
	if _, err := fixture.call(wide, "issue_create", map[string]any{"title": "x", "body": "b"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("issue without upstream for two scopes = %v", err)
	}
	// An unscoped uplink may file under an upstream, and scoped ones see it.
	owner := mustCall(t, fixture, unscopedActor, "issue_create", map[string]any{"title": "for laptop", "body": "b", "upstream": "github"}).(api.IssueCreateResponse)
	if _, err := fixture.call(scopedActor, "issue_get", map[string]any{"id": owner.ID}); err != nil {
		t.Fatalf("scoped read of an issue filed under its upstream: %v", err)
	}
}

func TestUnscopedUplinkSeesEverything(t *testing.T) {
	fixture := newScopeFixture(t)
	value := mustCall(t, fixture, unscopedActor, "run_list", map[string]any{})
	if runs := value.(api.RunListResponse).Runs.([]model.Run); len(runs) != len(fixture.runs["github"])+len(fixture.runs["codeberg"]) {
		t.Fatalf("unscoped run_list = %d", len(runs))
	}
	if _, err := fixture.call(unscopedActor, "issue_get", map[string]any{"id": fixture.global.ID}); err != nil {
		t.Fatalf("unscoped global issue: %v", err)
	}
	if _, err := fixture.call(unscopedActor, "system_status", map[string]any{}); err != nil || fixture.health.scopedFor != nil {
		t.Fatalf("unscoped system_status used the scoped view: %v", err)
	}
}

// A scoped actor without the scope capabilities fails closed rather than
// falling back to the unscoped view.
func TestScopedActorFailsClosedWithoutScopeSource(t *testing.T) {
	service, err := NewAPI(APIConfig{Runs: bareRunResolver{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.CallTool(context.Background(), scopedActor, "run_list", json.RawMessage(`{}`)); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("scoped call without a scope source = %v", err)
	}
	if _, err := service.Status(context.Background(), scopedActor); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("scoped status without scoped health = %v", err)
	}
}

type bareRunResolver struct{}

func (bareRunResolver) RepositoryByName(context.Context, string) (model.Repository, error) {
	return model.Repository{}, store.ErrNotFound
}
func (bareRunResolver) ListRepositories(context.Context) ([]model.Repository, error) { return nil, nil }
func (bareRunResolver) ResolveRun(context.Context, int64, string) (model.Run, error) {
	return model.Run{}, store.ErrNotFound
}
func (bareRunResolver) ResolveWaitRun(context.Context, int64, string, string) (model.Run, error) {
	return model.Run{}, store.ErrNotFound
}

// Every MCP tool must say how it treats a scoped uplink, so a new tool cannot
// silently answer from outside the scope. "filtered": lists and counts cover
// the scope; "lookup": out-of-scope records answer as missing; "mutation":
// refused outside the scope; "refused": installation-wide, never for a
// scoped uplink.
var toolScopeBehaviour = map[string]string{
	"status": "lookup", "logs": "lookup", "run_kvm_preflight": "lookup", "run_get": "lookup",
	"artifacts": "lookup", "artifact_get": "lookup", "run_logs": "lookup", "wait": "lookup",
	"promote_status": "lookup", "issue_get": "lookup", "issue_get_many": "lookup",
	"run_list": "filtered", "promotion_list": "filtered", "issue_list": "filtered",
	"access_list": "filtered", "repo_list": "filtered", "promote_grant_list": "filtered",
	"kubevirt_grant_get": "lookup",
	"system_status":      "filtered",
	"sync":               "mutation", "promote": "mutation", "publish_retry": "mutation", "run_retry": "mutation",
	"run_cancel":   "mutation",
	"issue_create": "mutation", "issue_update": "mutation", "issue_append": "mutation",
	"issue_close": "mutation", "issue_reopen": "mutation", "issue_delete": "mutation",
	"issue_lock": "mutation", "access_allow": "mutation", "access_revoke": "mutation",
	"kubevirt_grant": "mutation", "kubevirt_revoke": "mutation",
	"repo_remove": "mutation", "promote_grant": "mutation", "promote_grant_revoke": "mutation",
	"secretstore_verify": "refused", "secretstore_plan": "refused", "secretstore_sync_receipt": "refused",
}

// A new tool must be classified here AND covered by the behaviour tests:
// a lookup or mutation gets a hidden/missing pair in
// TestScopedUplinkCannotTellHiddenFromMissing, a filtered list a count and
// leak check in TestScopedUplinkSeesOnlyItsUpstream.
func TestEveryToolIsClassifiedForScopedUplinks(t *testing.T) {
	names := api.ToolNames()
	for _, name := range names {
		if _, ok := toolScopeBehaviour[name]; !ok {
			t.Errorf("MCP tool %q has no scope behaviour (#073): classify it in toolScopeBehaviour and cover it in the scope tests", name)
		}
	}
	for name := range toolScopeBehaviour {
		if !slices.Contains(names, name) {
			t.Errorf("toolScopeBehaviour names %q, which is no longer an MCP tool", name)
		}
	}
}

type busyAdmission struct{}

func (busyAdmission) AdmissionSnapshot(int64) *model.AdmissionObservation {
	return &model.AdmissionObservation{State: "waiting", Position: 3, Size: "M", Weight: 2, Used: 7, Capacity: 8,
		DiskFloor: &model.DiskFloorObservation{BelowFloor: true, FreeBytes: 1, FloorBytes: 2}}
}

// A scoped actor's waiting run says only that capacity is short: the queue
// position, the capacity in use and the disk floor describe invisible work
// too (#073).
func TestScopedSchedulingHidesInstallationLoad(t *testing.T) {
	scoped := &API{admission: busyAdmission{}, scope: &repoScope{}}
	unscoped := &API{admission: busyAdmission{}}
	run := model.Run{ID: "run", Status: model.RunRunning, QueueSequence: 5}
	got := scoped.scheduling(context.Background(), run, nil)
	if got.Admission.Position != 0 || got.Admission.Used != 0 || got.Admission.Capacity != 0 || got.Admission.DiskFloor != nil ||
		got.Message != "Waiting for capacity." || got.Admission.Size != "M" {
		t.Fatalf("scoped scheduling = %#v %#v", got, got.Admission)
	}
	if full := unscoped.scheduling(context.Background(), run, nil); full.Admission.Used != 7 || !strings.Contains(full.Message, "disk space") {
		t.Fatalf("unscoped scheduling = %#v %#v", full, full.Admission)
	}
}
