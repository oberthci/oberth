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
		return MergeCandidate{}, fmt.Errorf("fetch promotion target %s: %w", targetBranch, err)
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

// PeelObject returns both the immutable raw object identity and the commit it
// selects. Annotated tags keep their tag-object SHA for SyncTag.
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
	commit, err := c.capture(ctx, path, "rev-parse", "--verify", objectSHA+"^{commit}")
	if err != nil {
		return PeeledObject{}, fmt.Errorf("peel object %s to commit: %w", objectSHA, err)
	}
	commit = strings.TrimSpace(commit)
	if err := ValidateSHA(commit); err != nil {
		return PeeledObject{}, err
	}
	return PeeledObject{ObjectSHA: objectSHA, CommitSHA: commit}, nil
}

func (c *Cache) isAncestor(ctx context.Context, path, older, newer string) bool {
	return c.run(ctx, commandSpec{dir: path, args: []string{"merge-base", "--is-ancestor", older, newer}}) == nil
}
