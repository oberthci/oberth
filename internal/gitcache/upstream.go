package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/oberthci/oberth/internal/boundedio"
)

// SyncBranch force-publishes an exact branch SHA and verifies the remote ref.
func (c *Cache) SyncBranch(ctx context.Context, input, branch, sha string) error {
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
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
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
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
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	receive := c.receiveLock(path)
	if err := receive.LockContext(ctx); err != nil {
		return err
	}
	defer receive.Unlock()
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if err := c.allowBranchDeletion(ctx, path, ref); err != nil {
		return err
	}
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
	_, path, err := c.path(input)
	if err != nil {
		return false, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return false, err
	}
	defer lock.RUnlock()
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
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return false, err
	}
	defer lock.RUnlock()
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
	_, path, err := c.path(input)
	if err != nil {
		return MergeCandidate{}, err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return MergeCandidate{}, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return MergeCandidate{}, err
	}
	if err := c.assertCommit(ctx, path, sourceSHA); err != nil {
		return MergeCandidate{}, err
	}
	tracking := "refs/oberth/promotion-targets/" + targetBranch
	defer func() { _ = c.deleteRef(context.WithoutCancel(ctx), path, tracking, "") }()
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
	// A source may be retained only by a private promotion/receive ref. Fetch
	// it explicitly: the in-process workspace clone copies public refs only.
	if err := c.run(ctx, commandSpec{dir: workspace, args: []string{"fetch", "--quiet", "--force", "origin", sourceSHA}}); err != nil {
		return MergeCandidate{}, fmt.Errorf("fetch promotion source: %w", err)
	}
	if err := c.mergePromotionTrees(ctx, workspace, sourceSHA, targetBranch); err != nil {
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
	ref := promotionRefPrefix + targetBranch + "/" + sha
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
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
	if err := c.assertCommit(ctx, path, mergedSHA); err != nil {
		return err
	}
	ref := "refs/heads/" + targetBranch
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"push", "upstream", mergedSHA + ":" + ref}}); err != nil {
		return fmt.Errorf("push promotion target %s without force: %w", targetBranch, err)
	}
	if err := c.verifyRemoteRef(ctx, path, "--heads", ref, mergedSHA); err != nil {
		return err
	}
	return c.deleteRef(ctx, path, promotionRefPrefix+targetBranch+"/"+mergedSHA, "")
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
	_, path, err := c.path(input)
	if err != nil {
		return PeeledObject{}, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return PeeledObject{}, err
	}
	defer lock.RUnlock()
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
	_, path, err := c.path(input)
	if err != nil {
		return 0, err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return 0, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return 0, err
	}
	remote, err := c.validatedUpstream(input)
	if err != nil {
		return 0, err
	}
	ctx, cancelOutput := context.WithCancel(ctx)
	defer cancelOutput()
	output := boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancelOutput}
	err = c.run(ctx, commandSpec{repository: input, args: []string{"ls-remote", "--heads", remote}, stdout: &output})
	if err != nil {
		return 0, fmt.Errorf("ls-remote probe for %s: %w", input, err)
	}
	count := 0
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		if line != "" {
			fields := strings.Fields(line)
			if len(fields) != 2 || ValidateSHA(fields[0]) != nil || !strings.HasPrefix(fields[1], "refs/heads/") {
				return 0, errors.New("ls-remote returned an invalid branch row")
			}
			count++
		}
	}
	return count, nil
}

const promotionRefPrefix = "refs/oberth/promotions/"

// Move old pins before materialization so private promotion state never becomes
// part of the upstream snapshot. Keep undelivered objects pinned across restart.
func (c *Cache) migratePromotionPins(ctx context.Context, path string) error {
	legacy, err := c.listRefs(ctx, path, upstreamRefPrefix+"promotions/")
	if err != nil {
		return err
	}
	for ref, sha := range legacy {
		suffix := strings.TrimPrefix(ref, upstreamRefPrefix+"promotions/")
		// The last component is the object ID; branch names may contain slashes.
		split := strings.LastIndex(suffix, "/")
		if split < 1 || suffix[split+1:] != sha {
			return fmt.Errorf("invalid legacy promotion pin %s", ref)
		}
		target := suffix[:split]
		if !c.isAncestor(ctx, path, sha, upstreamRefPrefix+"heads/"+target) {
			if err := c.updateRef(ctx, path, promotionRefPrefix+suffix, sha, ""); err != nil {
				return err
			}
		}
		if err := c.deleteRef(ctx, path, ref, sha); err != nil {
			return err
		}
	}
	// The old preparation ref was only scratch state, never an ownership pin.
	scratch, err := c.listRefs(ctx, path, upstreamRefPrefix+"promote/")
	if err != nil {
		return err
	}
	for ref, sha := range scratch {
		if err := c.deleteRef(ctx, path, ref, sha); err != nil {
			return err
		}
	}
	return nil
}

// Remove pins proven delivered or abandoned in the publication ledger. Other
// candidates retain their object lifetime across restarts and retries.
func (c *Cache) sweepDeliveredPromotionPins(ctx context.Context, input, path string) error {
	pins, err := c.listRefs(ctx, path, promotionRefPrefix)
	if err != nil {
		return err
	}
	for ref, sha := range pins {
		suffix := strings.TrimPrefix(ref, promotionRefPrefix)
		split := strings.LastIndex(suffix, "/")
		if split < 1 || suffix[split+1:] != sha {
			return fmt.Errorf("invalid promotion pin %s", ref)
		}
		remove := c.isAncestor(ctx, path, sha, upstreamRefPrefix+"heads/"+suffix[:split])
		if !remove && c.promotionPinAbandoned != nil {
			remove, err = c.promotionPinAbandoned(ctx, input, suffix[:split], sha)
			if err != nil {
				return fmt.Errorf("check abandoned promotion pin: %w", err)
			}
		}
		if remove {
			if err := c.deleteRef(ctx, path, ref, sha); err != nil {
				return err
			}
		}
	}
	return nil
}
