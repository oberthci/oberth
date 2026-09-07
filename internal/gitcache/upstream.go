package gitcache

import (
	"context"
	"fmt"
	"os"
	"strings"
)

// SyncBranch force-publishes an exact branch SHA and verifies the remote ref.
func (c *Cache) SyncBranch(ctx context.Context, input, branch, sha string) error {
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertCommit(ctx, path, sha); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "--force", "upstream", sha + ":" + ref}}); err != nil {
		return fmt.Errorf("force-sync branch %s: %w", branch, err)
	}
	return c.verifyRemoteRef(ctx, path, "--heads", ref, sha)
}

// SyncTag publishes an immutable tag object or commit and verifies the exact
// remote tag ref. It deliberately does not force tag movement.
func (c *Cache) SyncTag(ctx context.Context, input, tag, sha string) error {
	if err := ValidateTag(tag); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertObject(ctx, path, sha); err != nil {
		return err
	}
	ref := "refs/tags/" + tag
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "upstream", sha + ":" + ref}}); err != nil {
		return fmt.Errorf("sync tag %s: %w", tag, err)
	}
	return c.verifyRemoteRef(ctx, path, "--tags", ref, sha)
}

// DeleteBranch removes a branch from the upstream forge and cleans up the
// local tracking and public refs. If the branch does not exist upstream,
// only local cleanup is performed — a never-published branch is not an error.
func (c *Cache) DeleteBranch(ctx context.Context, input, branch string) error {
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	ref := "refs/heads/" + branch
	// Check whether the branch exists upstream before pushing the deletion
	// so a never-published branch completes silently.
	output, err := c.capture(ctx, path, "ls-remote", "--heads", "upstream", ref)
	if err != nil {
		return fmt.Errorf("check upstream branch %s: %w", branch, err)
	}
	if strings.TrimSpace(output) != "" {
		if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "upstream", ":" + ref}}); err != nil {
			return fmt.Errorf("delete upstream branch %s: %w", branch, err)
		}
	}
	// Clean up the local tracking and public refs so a later refresh
	// does not re-create the deleted branch from stale upstream state.
	tracking := upstreamRefPrefix + "heads/" + branch
	_ = c.deleteRef(ctx, path, tracking, "")
	_ = c.deleteRef(ctx, path, ref, "")
	return nil
}

// ReleaseReachable proves commit ancestry against the exact upstream snapshot
// durably bound to the receive before its public ref mutation. It never trusts
// the client-writable public default branch or later repository metadata.
func (c *Cache) ReleaseReachable(ctx context.Context, input, commit, admissionSHA string) (bool, error) {
	if err := ValidateSHA(commit); err != nil {
		return false, err
	}
	if err := ValidateSHA(admissionSHA); err != nil {
		return false, err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return false, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertCommit(ctx, path, admissionSHA); err != nil {
		return false, fmt.Errorf("release admission snapshot is unavailable: %w", err)
	}
	err = c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", commit, admissionSHA}})
	if err == nil {
		return true, nil
	}
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

// ReachableFromUpstreamDefault checks whether a commit is an ancestor of the
// cached upstream default branch. Unlike ReleaseReachable — which uses the
// exact admission snapshot bound to a specific receive — this uses the latest
// upstream tracking ref, refreshed at the last Ensure. It is the fragment
// reachability gate (#213): a cross-repo pipeline fragment's tag commit must
// be reachable from its source repository's upstream default branch before a
// release-tier pipeline may inline it.
//
// Returns true if the commit is an ancestor, false if not, or an error if the
// repository is not cached or the commit is not present.
func (c *Cache) ReachableFromUpstreamDefault(ctx context.Context, input, commit string) (bool, error) {
	if err := ValidateSHA(commit); err != nil {
		return false, err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return false, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return false, fmt.Errorf("repository %s is not cached", repo)
	}
	branch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return false, fmt.Errorf("resolve default branch for %s: %w", repo, err)
	}
	tracking := upstreamRefPrefix + "heads/" + branch
	tipSHA, err := c.capture(ctx, path, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return false, fmt.Errorf("upstream default branch %s is not tracked for %s: %w", branch, repo, err)
	}
	tipSHA = strings.TrimSpace(tipSHA)
	if err := ValidateSHA(tipSHA); err != nil {
		return false, err
	}
	if err := c.assertCommit(ctx, path, commit); err != nil {
		return false, fmt.Errorf("fragment commit %s is not present in %s cache: %w", commit, repo, err)
	}
	err = c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", commit, tipSHA}})
	if err == nil {
		return true, nil
	}
	if isExitCode(err, 1) {
		return false, nil
	}
	return false, err
}

// PreparePromotion fetches the exact upstream target and creates a local merge
// candidate. A fast-forward reuses an already-tested source SHA.
func (c *Cache) PreparePromotion(ctx context.Context, input, sourceSHA, targetBranch, workspace string) (MergeCandidate, error) {
	if err := ValidateSHA(sourceSHA); err != nil {
		return MergeCandidate{}, err
	}
	if err := ValidateBranch(targetBranch); err != nil {
		return MergeCandidate{}, err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return MergeCandidate{}, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertCommit(ctx, path, sourceSHA); err != nil {
		return MergeCandidate{}, err
	}
	tracking := upstreamRefPrefix + "promote/" + targetBranch
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"fetch", "--no-tags", "upstream", "+refs/heads/" + targetBranch + ":" + tracking}}); err != nil {
		unborn, unbornErr := c.remoteBranchAbsent(ctx, path, targetBranch)
		if unbornErr != nil || !unborn {
			return MergeCandidate{}, fmt.Errorf("fetch promotion target %s: %w", targetBranch, err)
		}
		// The upstream repository has no target branch yet (brand-new
		// repository, issue #264 bootstrap chain): the promotion is a
		// branch creation that publishes the already-tested source SHA
		// verbatim. A stale tracking ref from an earlier prepared cycle
		// must not linger as a phantom base.
		_ = c.run(ctx, commandSpec{dir: path, args: []string{"update-ref", "-d", tracking}})
		if err := c.pinPromotion(ctx, path, targetBranch, sourceSHA, ""); err != nil {
			return MergeCandidate{}, err
		}
		return MergeCandidate{MergedSHA: sourceSHA, FastForward: true, TargetUnborn: true}, nil
	}
	base, err := c.capture(ctx, path, "rev-parse", "--verify", tracking+"^{commit}")
	if err != nil {
		return MergeCandidate{}, err
	}
	base = strings.TrimSpace(base)
	if err := ValidateSHA(base); err != nil {
		return MergeCandidate{}, fmt.Errorf("promotion base: %w", err)
	}
	if c.isAncestor(ctx, path, base, sourceSHA) {
		if err := c.pinPromotion(ctx, path, targetBranch, sourceSHA, ""); err != nil {
			return MergeCandidate{}, err
		}
		return MergeCandidate{BaseSHA: base, MergedSHA: sourceSHA, FastForward: true}, nil
	}
	if c.isAncestor(ctx, path, sourceSHA, base) {
		if err := c.pinPromotion(ctx, path, targetBranch, base, ""); err != nil {
			return MergeCandidate{}, err
		}
		return MergeCandidate{BaseSHA: base, MergedSHA: base, FastForward: false}, nil
	}
	if err := c.checkoutLocked(ctx, path, base, workspace); err != nil {
		return MergeCandidate{}, err
	}
	clean := false
	defer func() {
		if !clean {
			_ = os.RemoveAll(workspace)
		}
	}()
	if err := c.run(ctx, commandSpec{dir: workspace, args: []string{
		"-c", "user.name=Oberth",
		"-c", "user.email=oberth@localhost",
		"-c", "commit.gpgsign=false",
		"merge", "--no-ff", "--no-edit", sourceSHA,
	}}); err != nil {
		return MergeCandidate{}, fmt.Errorf("merge promotion candidate: %w", err)
	}
	merged, err := c.capture(ctx, workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return MergeCandidate{}, err
	}
	merged = strings.TrimSpace(merged)
	if err := ValidateSHA(merged); err != nil {
		return MergeCandidate{}, err
	}
	if err := c.pinPromotion(ctx, path, targetBranch, merged, workspace); err != nil {
		return MergeCandidate{}, err
	}
	clean = true
	return MergeCandidate{BaseSHA: base, MergedSHA: merged}, nil
}

// remoteBranchAbsent reports whether the upstream definitively lacks the
// branch: ls-remote must itself succeed (transport reachable) and return no
// matching ref, so a network failure can never masquerade as an unborn
// target and turn a transient outage into a branch creation.
func (c *Cache) remoteBranchAbsent(ctx context.Context, path, branch string) (bool, error) {
	output, err := c.capture(ctx, path, "ls-remote", "--heads", "upstream", "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(output) == "", nil
}

func (c *Cache) pinPromotion(ctx context.Context, cachePath, targetBranch, sha, workspace string) error {
	ref := upstreamRefPrefix + "promotions/" + targetBranch + "/" + sha
	if workspace != "" {
		if err := c.run(ctx, commandSpec{dir: workspace, args: []string{"push", "origin", sha + ":" + ref}}); err != nil {
			return fmt.Errorf("persist merged promotion object in cache: %w", err)
		}
	} else if err := c.updateRef(ctx, cachePath, ref, sha, ""); err != nil {
		return fmt.Errorf("pin promotion object in cache: %w", err)
	}
	return c.assertCommit(ctx, cachePath, sha)
}

// PushPromotion uses a normal non-forced push. If the target moved after
// PreparePromotion, Git's non-fast-forward rejection is the concurrency guard.
func (c *Cache) PushPromotion(ctx context.Context, input, targetBranch, mergedSHA string) error {
	if err := ValidateBranch(targetBranch); err != nil {
		return err
	}
	if err := ValidateSHA(mergedSHA); err != nil {
		return err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertCommit(ctx, path, mergedSHA); err != nil {
		return err
	}
	ref := "refs/heads/" + targetBranch
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "upstream", mergedSHA + ":" + ref}}); err != nil {
		return fmt.Errorf("push promotion target %s without force: %w", targetBranch, err)
	}
	return c.verifyRemoteRef(ctx, path, "--heads", ref, mergedSHA)
}

func (c *Cache) verifyRemoteRef(ctx context.Context, path, selector, ref, expected string) error {
	output, err := c.capture(ctx, path, "ls-remote", selector, "upstream", ref)
	if err != nil {
		return fmt.Errorf("verify upstream %s: %w", ref, err)
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == ref && fields[0] == expected {
			return nil
		}
	}
	return fmt.Errorf("upstream %s does not resolve to expected SHA %s", ref, expected)
}

func (c *Cache) assertCommit(ctx context.Context, path, sha string) error {
	if _, err := c.capture(ctx, path, "cat-file", "-e", sha+"^{commit}"); err != nil {
		return fmt.Errorf("commit %s is not present in cache: %w", sha, err)
	}
	return nil
}

func (c *Cache) assertObject(ctx context.Context, path, sha string) error {
	if _, err := c.capture(ctx, path, "cat-file", "-e", sha+"^{object}"); err != nil {
		return fmt.Errorf("object %s is not present in cache: %w", sha, err)
	}
	return nil
}

// PeelObject returns the immutable raw object identity, the commit it selects,
// and the Git object type. Annotated tags keep their tag-object SHA for
// SyncTag; release admission uses ObjectType to enforce the annotated-tag
// provenance requirement (a lightweight tag is type "commit", not "tag").
func (c *Cache) PeelObject(ctx context.Context, input, objectSHA string) (PeeledObject, error) {
	if err := ValidateSHA(objectSHA); err != nil {
		return PeeledObject{}, err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return PeeledObject{}, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := c.assertObject(ctx, path, objectSHA); err != nil {
		return PeeledObject{}, err
	}
	objectType, err := c.capture(ctx, path, "cat-file", "-t", objectSHA)
	if err != nil {
		return PeeledObject{}, fmt.Errorf("determine object type of %s: %w", objectSHA, err)
	}
	objectType = strings.TrimSpace(objectType)
	commit, err := c.capture(ctx, path, "rev-parse", "--verify", objectSHA+"^{commit}")
	if err != nil {
		return PeeledObject{}, fmt.Errorf("peel object %s to commit: %w", objectSHA, err)
	}
	commit = strings.TrimSpace(commit)
	if err := ValidateSHA(commit); err != nil {
		return PeeledObject{}, err
	}
	return PeeledObject{ObjectSHA: objectSHA, CommitSHA: commit, ObjectType: objectType}, nil
}

func (c *Cache) isAncestor(ctx context.Context, path, older, newer string) bool {
	return c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", older, newer}}) == nil
}

// LsRemoteHeads probes the upstream for a repository with git ls-remote
// --heads. It returns the number of branch refs visible. A failure means
// the upstream is unreachable, the repository does not exist, or the SSH
// key lacks read access — the exact conditions that would cause an opaque
// "exit status 128" on the first push. The error includes the forge's
// stderr (bounded and redacted by the execute path, issue #212 part 1),
// so the caller gets "Permission denied (publickey)" or "repository not
// found" instead of a bare exit code.
func (c *Cache) LsRemoteHeads(ctx context.Context, input string) (int, error) {
	remote, err := c.validatedUpstream(input)
	if err != nil {
		return 0, err
	}
	output, err := c.execute(ctx, commandSpec{args: []string{"ls-remote", "--heads", remote}}, true)
	if err != nil {
		return 0, fmt.Errorf("ls-remote probe for %s: %w", input, err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line != "" {
			count++
		}
	}
	return count, nil
}
