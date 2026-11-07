package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/model"
)

var pruneAdmin = api.Actor{Identity: "admin@host", Admin: true}

type pruneFixture struct {
	*controlFixture
	cache    *gitcache.Cache
	service  *API
	upstream string
	work     string
	mirror   string
	input    string
	initial  string
	unmerged string
	queued   int
}

func (f *pruneFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(t.Context(), "git", append([]string{"-C", dir, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false"}, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// newPruneFixture caches a repository whose upstream has main, three merged
// branches, ci/keep (merged) and one unmerged branch.
func newPruneFixture(t *testing.T) *pruneFixture {
	t.Helper()
	f := &pruneFixture{controlFixture: newControlFixture(t)}
	root := t.TempDir()
	f.work, f.upstream = filepath.Join(root, "work"), filepath.Join(root, "upstream.git")
	if err := os.Mkdir(f.work, 0o700); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.work, "init", "--initial-branch=main")
	f.git(t, f.work, "commit", "--allow-empty", "-m", "initial")
	f.initial = f.git(t, f.work, "rev-parse", "HEAD")
	f.git(t, f.work, "checkout", "-b", "feature/unmerged")
	f.git(t, f.work, "commit", "--allow-empty", "-m", "unmerged work")
	f.unmerged = f.git(t, f.work, "rev-parse", "HEAD")
	f.git(t, f.work, "checkout", "main")
	for _, branch := range []string{"feature/a", "feature/b", "feature/c", "ci/keep", "master"} {
		f.git(t, f.work, "branch", branch, f.initial)
	}
	f.git(t, f.work, "clone", "--bare", f.work, f.upstream)
	cache, err := gitcache.New(gitcache.Config{Root: filepath.Join(root, "cache"), AllowLocalUpstreams: true,
		ProtectedBranches: []string{"main", "master"},
		Upstream:          func(string) (string, error) { return f.upstream, nil }})
	if err != nil {
		t.Fatal(err)
	}
	f.cache = cache
	f.input, err = f.store.QualifiedRepoName(t.Context(), f.repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	ready, err := cache.Ensure(t.Context(), f.input)
	if err != nil {
		t.Fatal(err)
	}
	f.mirror = ready.Path
	service, err := NewAPI(APIConfig{
		Runs: f.store, History: f.store, Repositories: f.store, Issues: f.store, Promotions: f.store, PromotionRuns: f.store,
		Enqueues: f.scheduler, Git: f.controlFixture.git, Logs: f.logs, Auditor: f.store,
		BranchPrune: cache, BranchDeletions: f.store, DeletionQueued: func() { f.queued++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service = service
	return f
}

func (f *pruneFixture) prune(t *testing.T, actor api.Actor, arguments map[string]any) (BranchPruneResponse, error) {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.service.CallTool(t.Context(), actor, "branch_delete_many", raw)
	response, _ := value.(BranchPruneResponse)
	return response, err
}

func planBranches(entries []BranchPlanEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Branch)
	}
	return names
}

func TestBranchDeleteManyPlansConfirmsAndPropagates(t *testing.T) {
	f := newPruneFixture(t)
	selector := map[string]any{"repo": f.repo.Name, "merged_into": "main", "exclude_prefixes": []string{"ci/"}}
	plan, err := f.prune(t, pruneAdmin, selector)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.DryRun || plan.PlanID == "" || strings.Join(planBranches(plan.Branches), ",") != "feature/a,feature/b,feature/c" || len(plan.Unmerged) != 0 {
		t.Fatalf("dry run = %+v", plan)
	}
	if entry := plan.Branches[0]; entry.HeadSHA != f.initial || !entry.Merged || entry.PatchEquivalent == nil || !*entry.PatchEquivalent || entry.LastCommit == "" {
		t.Fatalf("plan entry = %+v", entry)
	}
	if got := f.git(t, f.mirror, "for-each-ref", "--format=%(refname)", "refs/heads/feature/"); strings.Count(got, "\n") != 3 {
		t.Fatalf("dry run changed refs: %s", got)
	}

	// Unmerged branches appear only with allow_unmerged, listed separately.
	withUnmerged, err := f.prune(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "merged_into": "main", "exclude_prefixes": []string{"ci/"}, "allow_unmerged": true})
	if err != nil || strings.Join(planBranches(withUnmerged.Unmerged), ",") != "feature/unmerged" || withUnmerged.PlanID == plan.PlanID {
		t.Fatalf("allow_unmerged plan = %+v, %v", withUnmerged, err)
	}

	// A moved head makes the confirmation stale and deletes nothing.
	f.git(t, f.mirror, "update-ref", "refs/heads/feature/b", f.unmerged)
	confirm := map[string]any{"repo": f.repo.Name, "merged_into": "main", "exclude_prefixes": []string{"ci/"}, "confirm": true, "plan_id": plan.PlanID}
	if _, err := f.prune(t, pruneAdmin, confirm); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale confirmation = %v", err)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/heads/feature/a"); got != f.initial {
		t.Fatalf("stale plan deleted feature/a: %s", got)
	}
	f.git(t, f.mirror, "update-ref", "refs/heads/feature/b", f.initial)

	executed, err := f.prune(t, pruneAdmin, confirm)
	if err != nil {
		t.Fatal(err)
	}
	if executed.Deleted != 3 || executed.BatchID == "" || f.queued != 1 {
		t.Fatalf("confirmed = %+v queued=%d", executed, f.queued)
	}
	for _, branch := range []string{"feature/a", "feature/b", "feature/c"} {
		if got := f.git(t, f.mirror, "for-each-ref", "refs/heads/"+branch); got != "" {
			t.Fatalf("%s still on mirror", branch)
		}
	}
	receipt := f.deletions(t, map[string]any{"batch": executed.BatchID})
	if len(receipt) != 3 || receipt[0].Status != "pending" || receipt[0].Source != "prune" {
		t.Fatalf("receipt = %+v", receipt)
	}

	propagator, err := NewBranchDeletionPropagator(f.store, f.cache, f.store, func(context.Context) error { return nil }, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	if finished, err := propagator.Propagate(t.Context()); err != nil || finished != 3 {
		t.Fatalf("propagate = %d, %v", finished, err)
	}
	for _, record := range f.deletions(t, map[string]any{"batch": executed.BatchID}) {
		if record.Status != "propagated" {
			t.Fatalf("record = %+v", record)
		}
	}
	if got := f.git(t, f.upstream, "for-each-ref", "--format=%(refname)", "refs/heads/"); strings.Contains(got, "feature/a") || !strings.Contains(got, "ci/keep") || !strings.Contains(got, "feature/unmerged") {
		t.Fatalf("upstream heads = %s", got)
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "branch.", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for _, action := range actions {
		counts[action.Action]++
		if action.Action == "branch.prune" && (!strings.Contains(action.Details, plan.PlanID) || action.Actor != pruneAdmin.Identity || !strings.Contains(action.Details, `"deleted":3`)) {
			t.Fatalf("prune audit = %+v", action)
		}
	}
	if counts["branch.prune"] != 1 || counts["branch.delete.upstream"] != 1 {
		t.Fatalf("audit actions = %v", counts)
	}
}

func (f *pruneFixture) deletions(t *testing.T, arguments map[string]any) []BranchDeletionView {
	t.Helper()
	raw, err := json.Marshal(arguments)
	if err != nil {
		t.Fatal(err)
	}
	value, err := f.service.CallTool(t.Context(), pruneAdmin, "branch_deletion_list", raw)
	if err != nil {
		t.Fatal(err)
	}
	return value.(map[string]any)["deletions"].([]BranchDeletionView)
}

func TestBranchDeleteManyRefusals(t *testing.T) {
	f := newPruneFixture(t)
	if _, err := f.prune(t, api.Actor{Identity: "dev@host"}, map[string]any{"repo": f.repo.Name, "merged_into": "main"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin = %v", err)
	}
	if _, err := f.prune(t, api.Actor{Identity: "scoped@host", Admin: true, Scope: []string{"codeberg"}}, map[string]any{"repo": f.repo.Name, "merged_into": "main"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("scoped admin = %v", err)
	}
	if _, err := f.prune(t, pruneAdmin, map[string]any{"repo": f.repo.Name}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing selector = %v", err)
	}
	if _, err := f.prune(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "merged_into": "feature/a"}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("non-default merged_into = %v", err)
	}
	explicit := map[string]any{"repo": f.repo.Name, "branches": []map[string]string{
		{"branch": "feature/a", "expected_sha": f.unmerged},
		{"branch": "feature/unmerged", "expected_sha": f.unmerged},
		{"branch": "main", "expected_sha": f.initial},
		{"branch": "feature/b", "expected_sha": f.initial},
	}}
	plan, err := f.prune(t, pruneAdmin, explicit)
	if err != nil {
		t.Fatal(err)
	}
	reasons := map[string]string{}
	for _, refusal := range plan.Refused {
		reasons[refusal.Branch] = refusal.Reason
	}
	if !strings.Contains(reasons["feature/a"], "head moved") || !strings.Contains(reasons["feature/unmerged"], "allow_unmerged") ||
		!strings.Contains(reasons["main"], "default branch") || strings.Join(planBranches(plan.Branches), ",") != "feature/b" {
		t.Fatalf("explicit plan = %+v", plan)
	}
	// Selectors never pick protected branches, even with allow_unmerged.
	all, err := f.prune(t, pruneAdmin, map[string]any{"repo": f.repo.Name, "older_than": "1h", "allow_unmerged": true})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range append(all.Branches, all.Unmerged...) {
		if entry.Branch == "main" {
			t.Fatalf("selector picked the default branch: %+v", all)
		}
	}
	// The default branch is refused even with its typed confirmation.
	defaultBranch := map[string]any{"repo": f.repo.Name, "branches": []map[string]string{{"branch": "main", "expected_sha": f.initial}}, "confirm_protected": f.input + ":main"}
	if plan, err := f.prune(t, pruneAdmin, defaultBranch); err != nil || len(plan.Branches) != 0 || len(plan.Refused) != 1 || !strings.Contains(plan.Refused[0].Reason, "default branch") {
		t.Fatalf("default branch plan = %+v, %v", plan, err)
	}
	// One configured protected branch, alone, with the typed confirmation.
	single := map[string]any{"repo": f.repo.Name, "branches": []map[string]string{{"branch": "master", "expected_sha": f.initial}}, "confirm_protected": "wrong:master"}
	if plan, err := f.prune(t, pruneAdmin, single); err != nil || len(plan.Branches) != 0 || len(plan.Refused) != 1 {
		t.Fatalf("mistyped confirmation = %+v, %v", plan, err)
	}
	single["confirm_protected"] = f.input + ":master"
	plan, err = f.prune(t, pruneAdmin, single)
	if err != nil || strings.Join(planBranches(plan.Branches), ",") != "master" || !plan.Branches[0].Protected {
		t.Fatalf("confirmed protected plan = %+v, %v", plan, err)
	}
	single["confirm"], single["plan_id"] = true, plan.PlanID
	if executed, err := f.prune(t, pruneAdmin, single); err != nil || executed.Deleted != 1 {
		t.Fatalf("confirmed protected deletion = %+v, %v", executed, err)
	}
	if got := f.git(t, f.mirror, "rev-parse", "refs/heads/main"); got != f.initial {
		t.Fatalf("default branch changed: %s", got)
	}
}

// Queued prune rows are not due until the local deletion has run: a
// propagator pass in between neither skips them nor touches upstream.
func TestPropagatorBetweenPruneQueueAndLocalDeletion(t *testing.T) {
	f := newPruneFixture(t)
	propagator, err := NewBranchDeletionPropagator(f.store, f.cache, f.store, func(context.Context) error { return nil }, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	entries := []model.BranchPruneEntry{{Branch: "feature/a", SHA: f.initial}, {Branch: "feature/b", SHA: f.unmerged}}
	if err := f.store.QueueBranchPrune(t.Context(), "prune-test", f.repo.ID, pruneAdmin.Identity, entries); err != nil {
		t.Fatal(err)
	}
	if finished, err := propagator.Propagate(t.Context()); err != nil || finished != 0 {
		t.Fatalf("early pass = %d, %v", finished, err)
	}
	for _, record := range f.deletions(t, map[string]any{"batch": "prune-test"}) {
		if record.Status != "pending" || record.Attempts != 0 {
			t.Fatalf("early pass touched %+v", record)
		}
	}
	outcomes, err := f.cache.PruneBranches(t.Context(), f.input, []gitcache.BranchHead{{Branch: "feature/a", SHA: f.initial}, {Branch: "feature/b", SHA: f.unmerged}}, false)
	if err != nil {
		t.Fatal(err)
	}
	skipped := map[string]string{}
	for _, outcome := range outcomes {
		if outcome.Result != gitcache.PruneDeleted {
			skipped[outcome.Branch] = outcome.Result
		}
	}
	if len(skipped) != 1 || skipped["feature/b"] != gitcache.PruneMoved {
		t.Fatalf("outcomes = %+v", outcomes)
	}
	if err := f.store.FinishBranchPrune(t.Context(), "prune-test", pruneAdmin.Identity, f.repo.ID, skipped, map[string]any{"plan_id": "test"}); err != nil {
		t.Fatal(err)
	}
	if finished, err := propagator.Propagate(t.Context()); err != nil || finished != 1 {
		t.Fatalf("pass = %d, %v", finished, err)
	}
	states := map[string]string{}
	for _, record := range f.deletions(t, map[string]any{"batch": "prune-test"}) {
		states[record.Branch] = record.Status
	}
	if states["feature/a"] != "propagated" || states["feature/b"] != "skipped" {
		t.Fatalf("states = %v", states)
	}
	if got := f.git(t, f.upstream, "for-each-ref", "refs/heads/feature/a"); got != "" {
		t.Fatalf("upstream kept feature/a: %s", got)
	}
	if got := f.git(t, f.upstream, "rev-parse", "refs/heads/feature/b"); got != f.initial {
		t.Fatalf("upstream feature/b changed: %s", got)
	}
}

type fakeOutbox struct {
	record    gitcache.ReceiveRecord
	retried   int
	abandoned int
}

func (o *fakeOutbox) PendingReceives() ([]gitcache.PendingReceive, error) {
	return []gitcache.PendingReceive{{Repo: o.record.Repo, State: "quarantined"}}, nil
}
func (o *fakeOutbox) InspectReceiveRecord(string) (gitcache.ReceiveRecord, error) {
	return o.record, nil
}
func (o *fakeOutbox) RetryReceiveRecord(context.Context, string, string, gitcache.ReceiveHandler) (gitcache.ReceiveRecord, error) {
	o.retried++
	return o.record, nil
}
func (o *fakeOutbox) AbandonReceiveRecord(context.Context, string, string) (gitcache.ReceiveRecord, error) {
	o.abandoned++
	return o.record, nil
}

func TestReceiveOutboxRepairIsAdminAndAudited(t *testing.T) {
	f := newControlFixture(t)
	service := f.api(t)
	outbox := &fakeOutbox{record: gitcache.ReceiveRecord{Repo: "codeberg/acme/oberth", ID: "record-1", Actor: "operator@host", State: "quarantined", Corrupt: true, Retryable: true,
		Updates: []gitcache.RefUpdate{{Ref: "refs/heads/chore/917-followups", OldSHA: strings.Repeat("a", 40)}}}}
	service.SetReceiveOutbox(outbox, gitcache.ReceiveHandlerFunc(func(context.Context, gitcache.ReceiveEvent) error { return nil }))
	call := func(actor api.Actor, arguments map[string]any) (any, error) {
		raw, _ := json.Marshal(arguments)
		return service.CallTool(t.Context(), actor, "receive_outbox", raw)
	}
	if _, err := call(api.Actor{Identity: "dev@host"}, map[string]any{"action": "list"}); !errors.Is(err, ErrForbidden) {
		t.Fatalf("non-admin list = %v", err)
	}
	if value, err := call(pruneAdmin, map[string]any{"action": "show", "repo": outbox.record.Repo}); err != nil || value.(gitcache.ReceiveRecord).ID != "record-1" {
		t.Fatalf("show = %+v, %v", value, err)
	}
	if _, err := call(pruneAdmin, map[string]any{"action": "retry", "repo": outbox.record.Repo}); err != nil || outbox.retried != 1 {
		t.Fatalf("retry = %v (%d)", err, outbox.retried)
	}
	if _, err := call(pruneAdmin, map[string]any{"action": "abandon", "repo": outbox.record.Repo, "reason": "already applied"}); !errors.Is(err, ErrInvalidInput) || outbox.abandoned != 0 {
		t.Fatalf("abandon without id = %v", err)
	}
	if _, err := call(pruneAdmin, map[string]any{"action": "abandon", "repo": outbox.record.Repo, "id": "record-1", "reason": "already applied"}); err != nil || outbox.abandoned != 1 {
		t.Fatalf("abandon = %v", err)
	}
	actions, _, err := f.store.ListAuditActions(t.Context(), model.AuditListFilter{ActionPrefix: "receive_outbox.", Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, action := range actions {
		seen[action.Action] = true
	}
	if !seen["receive_outbox.retry.succeeded"] || !seen["receive_outbox.abandon.succeeded"] {
		t.Fatalf("audit = %v", seen)
	}
}
