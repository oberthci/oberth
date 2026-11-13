package gitcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// deletionFixture caches a repository whose upstream and mirror both carry
// feature/gone.
func deletionFixture(t *testing.T, root string) (*testRepository, *Cache, Repository, string) {
	t.Helper()
	repository := newTestRepository(t)
	featureSHA := repository.featureCommit(t, "feature/gone", "gone\n")
	runGit(t, repository.work, "push", "origin", featureSHA+":refs/heads/feature/gone")
	cache := newTestCacheAt(t, root, repository.upstream)
	ready, err := cache.Ensure(t.Context(), "example")
	if err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, ready.Path, "refs/heads/feature/gone"); got != featureSHA {
		t.Fatalf("mirror feature = %s", got)
	}
	return repository, cache, ready, featureSHA
}

// Receive finalization writes the deletion marker under the repository
// lock, so it already exists when the callback runs: no refresh between
// receive-pack and the callback can re-create the branch. The callback itself
// needs no Git call (the v0.17.84-v0.17.95 hang re-took the receive lock
// there).
func TestReceiveDeletionMarkedBeforeCallback(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	// Re-pushing a branch clears a stale marker in the same finalization.
	runGit(t, ready.Path, "update-ref", deletingRefPrefix+"feature/again", repository.initialSHA)
	var delivered []ReceiveEvent
	err := cache.receiveTransaction(t.Context(), "example", ready.Path, "operator@host", func() error {
		if err := cache.deleteRef(t.Context(), ready.Path, "refs/heads/feature/gone", featureSHA); err != nil {
			return err
		}
		return cache.updateRef(t.Context(), ready.Path, "refs/heads/feature/again", repository.initialSHA, "")
	}, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error {
		if event.Update.Deleted() {
			if got := runGitOptional(t, ready.Path, "rev-parse", "--verify", deletingRefPrefix+"feature/gone"); got != featureSHA {
				return errors.New("deletion marker missing at callback time")
			}
		}
		delivered = append(delivered, event)
		return nil
	}))
	if err != nil {
		t.Fatalf("receive with deletion callback: %v", err)
	}
	if len(delivered) != 2 {
		t.Fatalf("delivered = %+v", delivered)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", deletingRefPrefix+"feature/again"); got != "" {
		t.Fatalf("re-pushed branch kept its deletion marker: %s", got)
	}
}

// A deletion left in the outbox by an interrupted process replays at startup
// within the scheduler's recovery budget, even with an upstream that would
// never answer: replay makes no network call. The deleted branch then stays
// deleted across a refresh until propagation completes.
func TestReplayPendingDeletionCompletesWithinBudget(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cache")
	repository, cache, ready, featureSHA := deletionFixture(t, root)
	interrupted := errors.New("process stopped")
	err := cache.receiveTransaction(t.Context(), "example", ready.Path, "operator@host", func() error {
		return cache.deleteRef(t.Context(), ready.Path, "refs/heads/feature/gone", featureSHA)
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return interrupted }))
	if !errors.Is(err, interrupted) {
		t.Fatalf("receive = %v", err)
	}

	restarted := newTestCacheAt(t, root, repository.upstream)
	budget := testtimeout.Scale(20 * time.Second)
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	defer cancel()
	started := time.Now()
	if err := restarted.ReplayPending(ctx, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil })); err != nil {
		t.Fatalf("replay: %v", err)
	}
	if elapsed := time.Since(started); elapsed > budget {
		t.Fatalf("replay took %s", elapsed)
	}
	if pending, err := restarted.PendingReceives(); err != nil || len(pending) != 0 {
		t.Fatalf("pending after replay = %+v, %v", pending, err)
	}

	// Unrelated upstream movement forces materialization: the deleted
	// branch must not come back from its still-present tracking ref.
	repository.commit(t, "advance", "advanced\n")
	if _, err := restarted.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", "refs/heads/feature/gone"); got != "" {
		t.Fatalf("deleted branch re-created by refresh: %s", got)
	}

	results, err := restarted.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{{Branch: "feature/gone", OldSHA: featureSHA}})
	if err != nil || len(results) != 1 || results[0].State != UpstreamPropagated {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := runGitOptional(t, "", "ls-remote", repository.upstream, "refs/heads/feature/gone"); got != "" {
		t.Fatalf("upstream still has the branch: %s", got)
	}
	for _, ref := range []string{deletingRefPrefix + "feature/gone", upstreamRefPrefix + "heads/feature/gone"} {
		if got := runGitOptional(t, ready.Path, "show-ref", "--verify", ref); got != "" {
			t.Fatalf("%s left behind: %s", ref, got)
		}
	}
}

// An operator-quarantined record (state edited to "quarantined", the escape
// hatch used during the v0.17.95 upgrade) is skipped without delivery and
// kept byte-for-byte.
func TestReplayPendingKeepsSkippingQuarantinedRecord(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "cache")
	repository, cache, ready, featureSHA := deletionFixture(t, root)
	interrupted := errors.New("process stopped")
	_ = cache.receiveTransaction(t.Context(), "example", ready.Path, "operator@host", func() error {
		return cache.deleteRef(t.Context(), ready.Path, "refs/heads/feature/gone", featureSHA)
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return interrupted }))
	file, err := cache.reservationPath("example")
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(body, &record); err != nil {
		t.Fatal(err)
	}
	record["state"] = "quarantined"
	edited, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, edited, 0o600); err != nil {
		t.Fatal(err)
	}

	restarted := newTestCacheAt(t, root, repository.upstream)
	for range 2 {
		ctx, cancel := context.WithTimeout(t.Context(), testtimeout.Scale(10*time.Second))
		err := restarted.ReplayPending(ctx, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error {
			t.Fatal("quarantined record delivered")
			return nil
		}))
		cancel()
		if err != nil {
			t.Fatalf("replay with quarantined record: %v", err)
		}
	}
	pending, err := restarted.PendingReceives()
	if err != nil || len(pending) != 1 || pending[0].State != "quarantined" {
		t.Fatalf("pending = %+v, %v", pending, err)
	}
	if kept, err := os.ReadFile(file); err != nil || string(kept) != string(edited) {
		t.Fatalf("quarantined record changed: %v", err)
	}
	if _, err := restarted.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err == nil {
		t.Fatal("prune mutated a quarantined repository")
	}
}

func TestPruneBranchesComparesEachHead(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	movedSHA := repository.featureCommit(t, "feature/moved", "moved\n")
	seedCacheObject(t, repository.work, ready.Path, movedSHA)
	runGit(t, ready.Path, "update-ref", "refs/heads/feature/moved", movedSHA)
	outcomes, err := cache.PruneBranches(t.Context(), "example", []BranchHead{
		{Branch: "feature/gone", SHA: featureSHA},
		{Branch: "feature/moved", SHA: repository.initialSHA},
		{Branch: "feature/absent", SHA: repository.initialSHA},
		{Branch: "main", SHA: repository.initialSHA},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{PruneDeleted, PruneMoved, PruneMissing, PruneReserved}
	for index, outcome := range outcomes {
		if outcome.Result != want[index] {
			t.Fatalf("outcomes = %+v, want %v", outcomes, want)
		}
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", "refs/heads/feature/gone"); got != "" {
		t.Fatalf("deleted head still present: %s", got)
	}
	for ref, sha := range map[string]string{"refs/heads/feature/moved": movedSHA, "refs/heads/main": repository.initialSHA, deletingRefPrefix + "feature/gone": featureSHA} {
		if got := remoteRef(t, ready.Path, ref); got != sha {
			t.Fatalf("%s = %s, want %s", ref, got, sha)
		}
	}
}

func TestPropagateRefusesUpstreamThatMovedAndRestoresOnAbandon(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	if _, err := cache.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err != nil {
		t.Fatal(err)
	}
	// Someone pushes new work straight to the forge after the deletion.
	runGit(t, repository.work, "checkout", "feature/gone")
	newer := repository.featureCommit(t, "feature/other", "other\n")
	runGit(t, repository.work, "push", "--force", "origin", newer+":refs/heads/feature/gone")
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{{Branch: "feature/gone", OldSHA: featureSHA}})
	if err != nil || len(results) != 1 || results[0].State != UpstreamRefused || !strings.Contains(results[0].Err.Error(), "moved") {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/gone"); got != newer {
		t.Fatalf("upstream changed: %s", got)
	}
	if err := cache.AbandonBranchDeletions(t.Context(), "example", []string{"feature/gone"}); err != nil {
		t.Fatal(err)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", deletingRefPrefix+"feature/gone"); got != "" {
		t.Fatalf("marker kept after abandon: %s", got)
	}
	if got := remoteRef(t, ready.Path, "refs/heads/feature/gone"); got != featureSHA {
		t.Fatalf("mirror branch not restored from tracking: %s", got)
	}
}

func TestPropagateSkipsBranchPushedAgain(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	if _, err := cache.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err != nil {
		t.Fatal(err)
	}
	// Pushed again through a receive, which drops the deletion marker.
	if err := cache.receiveTransaction(t.Context(), "example", ready.Path, "dev@host", func() error {
		return cache.updateRef(t.Context(), ready.Path, "refs/heads/feature/gone", repository.initialSHA, "")
	}, ReceiveHandlerFunc(func(context.Context, ReceiveEvent) error { return nil })); err != nil {
		t.Fatal(err)
	}
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{{Branch: "feature/gone", OldSHA: featureSHA}})
	if err != nil || len(results) != 1 || results[0].State != UpstreamSkipped {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/gone"); got != featureSHA {
		t.Fatalf("upstream changed: %s", got)
	}
}

func TestPruneManyBranchesInOneTransaction(t *testing.T) {
	t.Parallel()
	repository, cache, ready, _ := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	const count = 250
	var input strings.Builder
	heads := make([]BranchHead, 0, count)
	for index := range count {
		branch := fmt.Sprintf("stale/b%03d", index)
		heads = append(heads, BranchHead{Branch: branch, SHA: repository.initialSHA})
		input.WriteString("create refs/heads/" + branch + " " + repository.initialSHA + "\n")
	}
	runGitInput(t, ready.Path, input.String(), "update-ref", "--stdin")
	runGitInput(t, repository.upstream, input.String(), "update-ref", "--stdin")
	started := time.Now()
	outcomes, err := cache.PruneBranches(t.Context(), "example", heads, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, outcome := range outcomes {
		if outcome.Result != PruneDeleted {
			t.Fatalf("outcome = %+v", outcome)
		}
	}
	deletions := make([]UpstreamDeletion, 0, count)
	for _, head := range heads {
		deletions = append(deletions, UpstreamDeletion{Branch: head.Branch, OldSHA: head.SHA})
	}
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", deletions)
	if err != nil {
		t.Fatal(err)
	}
	for _, result := range results {
		if result.State != UpstreamPropagated {
			t.Fatalf("result = %+v", result)
		}
	}
	if elapsed := time.Since(started); elapsed > testtimeout.Scale(60*time.Second) {
		t.Fatalf("pruning %d branches took %s", count, elapsed)
	}
	if got := runGitOptional(t, "", "ls-remote", "--heads", repository.upstream, "refs/heads/stale/*"); got != "" {
		t.Fatalf("upstream kept stale branches: %.200s", got)
	}
}

func TestBranchInventoryClassifiesMergedAndPatchEquivalent(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	// feature/merged is an ancestor of main; feature/gone is not merged.
	runGit(t, ready.Path, "update-ref", "refs/heads/feature/merged", repository.initialSHA)
	inventory, err := cache.BranchInventory(t.Context(), "example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.DefaultBranch != "main" || inventory.Target != "main" || inventory.TargetSHA != repository.initialSHA {
		t.Fatalf("inventory = %+v", inventory)
	}
	byName := make(map[string]BranchInventoryEntry)
	for _, entry := range inventory.Branches {
		byName[entry.Branch] = entry
	}
	if entry := byName["feature/merged"]; !entry.Merged || entry.Ahead != 0 {
		t.Fatalf("merged entry = %+v", entry)
	}
	if entry := byName["feature/gone"]; entry.Merged || entry.Ahead != 1 || entry.SHA != featureSHA {
		t.Fatalf("unmerged entry = %+v", entry)
	}
	// A cherry-picked copy of feature/gone on main makes it patch-equivalent.
	runGit(t, repository.work, "checkout", "main")
	runGit(t, repository.work, "cherry-pick", featureSHA)
	picked := runGit(t, repository.work, "rev-parse", "HEAD")
	seedCacheObject(t, repository.work, ready.Path, picked)
	runGit(t, ready.Path, "update-ref", "refs/heads/main", picked)
	equivalence, err := cache.BranchPatchEquivalence(t.Context(), "example", picked, []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, 10)
	if err != nil || len(equivalence) != 1 || !equivalence["feature/gone"] {
		t.Fatalf("patch equivalence = %v, %v", equivalence, err)
	}
	if entry := byName["main"]; !entry.Reserved || !entry.Default {
		t.Fatalf("default entry = %+v", entry)
	}
}

// The mirror's main can run ahead of the forge while a fast-forward push is
// unpublished. A branch merged only into those commits must stay unmerged,
// and patch equivalence must be judged against the forge-confirmed head.
func TestBranchInventoryJudgesMergedAgainstForgeConfirmedTarget(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	runGit(t, ready.Path, "update-ref", "refs/heads/feature/merged", repository.initialSHA)
	// An unpublished merge of feature/gone lands on the mirror's main only.
	runGit(t, repository.work, "checkout", "main")
	runGit(t, repository.work, "merge", "--no-ff", "--no-edit", featureSHA)
	unpublished := runGit(t, repository.work, "rev-parse", "HEAD")
	runGit(t, repository.work, "reset", "--hard", repository.initialSHA)
	seedCacheObject(t, repository.work, ready.Path, unpublished)
	runGit(t, ready.Path, "update-ref", "refs/heads/main", unpublished)
	inventory, err := cache.BranchInventory(t.Context(), "example", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if inventory.TargetSHA != repository.initialSHA {
		t.Fatalf("target sha = %s, want the forge-confirmed %s", inventory.TargetSHA, repository.initialSHA)
	}
	byName := make(map[string]BranchInventoryEntry)
	for _, entry := range inventory.Branches {
		byName[entry.Branch] = entry
	}
	if entry := byName["feature/gone"]; entry.Merged || entry.Ahead != 1 {
		t.Fatalf("branch merged only into unpublished mirror main judged merged: %+v", entry)
	}
	if entry := byName["feature/merged"]; !entry.Merged {
		t.Fatalf("branch merged upstream judged unmerged: %+v", entry)
	}
	// A promotion caller can use the exact result it verified upstream,
	// without trusting or rewriting the stale tracking ref.
	verified, err := cache.BranchInventory(t.Context(), "example", "", unpublished)
	if err != nil {
		t.Fatal(err)
	}
	if verified.TargetSHA != unpublished {
		t.Fatalf("verified target = %s", verified.TargetSHA)
	}
	for _, entry := range verified.Branches {
		if entry.Branch == "feature/gone" && (!entry.Merged || entry.Ahead != 0) {
			t.Fatalf("published source judged unmerged: %+v", entry)
		}
	}
	for _, target := range []string{"main", strings.Repeat("f", 40)} {
		if _, err := cache.BranchInventory(t.Context(), "example", "", target); err == nil {
			t.Fatalf("invalid or missing exact target %q accepted", target)
		}
	}
	// Without a forge-confirmed head the plan refuses instead of guessing.
	runGit(t, ready.Path, "update-ref", "-d", upstreamRefPrefix+"heads/main")
	if _, err := cache.BranchInventory(t.Context(), "example", "", ""); err == nil || !strings.Contains(err.Error(), "no forge-confirmed head") {
		t.Fatalf("inventory without upstream head = %v", err)
	}
}

func TestReceiveHookRefusesConfiguredReservedBranchDeletion(t *testing.T) {
	r, c, _ := hookSecurityFixture(t)
	c.hookChild = true
	t.Setenv(reservedBranchesEnv, "master,release/*")
	for ref, refused := range map[string]bool{"refs/heads/master": true, "refs/heads/release/1.0": true, "refs/heads/feature/x": false, "refs/heads/main": true} {
		err := c.nativeUpdate(t.Context(), ref, r.initialSHA, zeroObjectID)
		if (err != nil) != refused {
			t.Fatalf("delete %s: err=%v, want refused=%v", ref, err, refused)
		}
	}
}

func runGitInput(t *testing.T, dir, input string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = dir
	command.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	command.Stdin = strings.NewReader(input)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}

func TestPruneRefusesDefaultBranchEvenWhenReservedAllowed(t *testing.T) {
	t.Parallel()
	repository, cache, ready, _ := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	cache.reservedBranches = []string{"release/*"}
	runGit(t, ready.Path, "update-ref", "refs/heads/release/1", repository.initialSHA)
	heads := []BranchHead{{Branch: "main", SHA: repository.initialSHA}, {Branch: "release/1", SHA: repository.initialSHA}}
	outcomes, err := cache.PruneBranches(t.Context(), "example", heads, false)
	if err != nil || outcomes[0].Result != PruneReserved || outcomes[1].Result != PruneReserved {
		t.Fatalf("unconfirmed = %+v, %v", outcomes, err)
	}
	outcomes, err = cache.PruneBranches(t.Context(), "example", heads, true)
	if err != nil || outcomes[0].Result != PruneReserved || outcomes[1].Result != PruneDeleted {
		t.Fatalf("confirmed = %+v, %v", outcomes, err)
	}
	if got := remoteRef(t, ready.Path, "refs/heads/main"); got != repository.initialSHA {
		t.Fatalf("default branch deleted: %s", got)
	}
}

// Several pending records for one branch (deleted, pushed again, deleted
// again) become one delete refspec: Git refuses a push naming one
// destination twice.
func TestPropagateGroupsRecordsOfOneBranch(t *testing.T) {
	t.Parallel()
	repository, cache, _, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	if _, err := cache.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err != nil {
		t.Fatal(err)
	}
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{
		{Branch: "feature/gone", OldSHA: repository.initialSHA},
		{Branch: "feature/gone", OldSHA: featureSHA},
	})
	if err != nil || len(results) != 2 || results[0].State != UpstreamPropagated || results[1].State != UpstreamPropagated {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := runGitOptional(t, "", "ls-remote", repository.upstream, "refs/heads/feature/gone"); got != "" {
		t.Fatalf("upstream still has the branch: %s", got)
	}
}

// A queued prune whose local deletion has not run yet (the branch is still on
// the mirror, with its marker) is deferred, never skipped.
func TestPropagateDefersUntilLocalDeletionApplies(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	runGit(t, ready.Path, "update-ref", deletingRefPrefix+"feature/gone", featureSHA)
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{{Branch: "feature/gone", OldSHA: featureSHA}})
	if err != nil || len(results) != 1 || results[0].State != UpstreamRetry {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/gone"); got != featureSHA {
		t.Fatalf("upstream changed: %s", got)
	}
	if got := remoteRef(t, ready.Path, deletingRefPrefix+"feature/gone"); got != featureSHA {
		t.Fatalf("marker dropped: %s", got)
	}
}

// A forge that refuses the push because the repository is archived ends the
// deletion at once: the upstream ref stays, the result is final, and the
// mirror keeps both the deletion and its marker, so a refresh does not bring
// the branch back from the read-only forge (#1500).
func TestPropagateTreatsArchivedUpstreamAsFinal(t *testing.T) {
	t.Parallel()
	repository, cache, ready, featureSHA := deletionFixture(t, filepath.Join(t.TempDir(), "cache"))
	if _, err := cache.PruneBranches(t.Context(), "example", []BranchHead{{Branch: "feature/gone", SHA: featureSHA}}, false); err != nil {
		t.Fatal(err)
	}
	// A repository initialised without templates (as the static Git runtime
	// does) has no hooks directory.
	if err := os.MkdirAll(filepath.Join(repository.upstream, "hooks"), 0o755); err != nil {
		t.Fatal(err)
	}
	hook := filepath.Join(repository.upstream, "hooks", "pre-receive")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\necho 'Forgejo: Repo \"example/example\" is archived.' >&2\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	// The static Git qualification lane's launcher refuses to execute anything
	// outside the candidate payload, so a forge hook cannot run there; the
	// ordinary gitcache lane runs this test with the hook executing.
	if err := exec.Command(hook).Run(); err != nil && errors.Is(err, os.ErrPermission) {
		t.Skip("forge hook cannot execute in this runtime (payload-only exec)")
	}
	results, err := cache.PropagateBranchDeletions(t.Context(), "example", []UpstreamDeletion{{Branch: "feature/gone", OldSHA: featureSHA}})
	if err != nil || len(results) != 1 || results[0].State != UpstreamArchived || !ArchivedUpstream(results[0].Err) {
		t.Fatalf("propagate = %+v, %v", results, err)
	}
	if got := remoteRef(t, repository.upstream, "refs/heads/feature/gone"); got != featureSHA {
		t.Fatalf("upstream changed: %s", got)
	}
	if got := runGitOptional(t, ready.Path, "rev-parse", "--verify", deletingRefPrefix+"feature/gone"); got != featureSHA {
		t.Fatalf("archived deletion dropped its marker: %q", got)
	}
	// Another upstream change makes the next refresh materialize again.
	if err := os.Remove(hook); err != nil {
		t.Fatal(err)
	}
	newer := repository.featureCommit(t, "feature/new", "new\n")
	runGit(t, repository.work, "push", "origin", newer+":refs/heads/feature/new")
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	if got := remoteRef(t, ready.Path, "refs/heads/feature/new"); got != newer {
		t.Fatalf("refresh did not materialize: %s", got)
	}
	if got := runGitOptional(t, ready.Path, "show-ref", "--verify", "refs/heads/feature/gone"); got != "" {
		t.Fatalf("refresh re-created the deleted branch: %s", got)
	}
}

func TestArchivedUpstreamMessage(t *testing.T) {
	t.Parallel()
	for message, want := range map[string]bool{
		`remote: Forgejo: Repo "org/name" is archived.`:                    true,
		"remote: Gitea: repo is archived\nerror: failed to push some refs": true,
		"ERROR: This repository was archived so it is read-only.":          true,
		"remote: error: GH006: pre-receive hook declined":                  false,
		"error: failed to push some refs to 'repo.git'\nis archived":       false,
	} {
		if got := ArchivedUpstream(errors.New(message)); got != want {
			t.Errorf("ArchivedUpstream(%q) = %t, want %t", message, got, want)
		}
	}
	if ArchivedUpstream(nil) {
		t.Error("nil error classified as archived")
	}
}
