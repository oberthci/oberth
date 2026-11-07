package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/boundedio"
)

// deletingRefPrefix holds one private ref per accepted branch deletion whose
// upstream propagation has not finished. It keeps the deleted head's objects
// reachable and stops materialize from re-creating the public branch from a
// still-present upstream tracking ref. Clients never see refs/oberth/.
const deletingRefPrefix = "refs/oberth/deleting/heads/"

// protectedBranchesEnv carries the configured protected-branch globs from the
// server into its receive hooks.
const protectedBranchesEnv = "OBERTH_PROTECTED_BRANCHES"

// upstreamDeletionChunk bounds the delete refspecs of one upstream push.
const upstreamDeletionChunk = 100

// ErrReceivePending refuses a prune while an earlier receive of the same
// repository is not yet delivered: its ref diff must not absorb the prune.
var ErrReceivePending = errors.New("an earlier receive of this repository is not yet delivered; retry shortly")

// ProtectedBranchPatterns normalizes configured protected-branch globs.
// Patterns use path.Match syntax; "*" does not cross "/".
func ProtectedBranchPatterns(values []string) ([]string, error) {
	patterns := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		if len(value) > maximumRefPartBytes {
			return nil, fmt.Errorf("protected branch pattern %q is too long", value)
		}
		if _, err := path.Match(value, ""); err != nil {
			return nil, fmt.Errorf("protected branch pattern %q: %w", value, err)
		}
		seen[value] = true
		patterns = append(patterns, value)
	}
	return patterns, nil
}

func protectedByPattern(branch string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, err := path.Match(pattern, branch); err == nil && matched {
			return true
		}
	}
	return false
}

func (c *Cache) protectedByConfig(branch string) bool {
	if c.hookChild {
		patterns, err := ProtectedBranchPatterns(strings.Split(os.Getenv(protectedBranchesEnv), ","))
		// An unreadable configuration cannot grant a deletion.
		return err != nil || protectedByPattern(branch, patterns)
	}
	return protectedByPattern(branch, c.protectedBranches)
}

// BranchProtected reports whether a branch is the default branch or matches
// a configured protected pattern.
func (c *Cache) BranchProtected(branch, defaultBranch string) bool {
	return branch == defaultBranch || c.protectedByConfig(branch)
}

// MarkBranchDeletion records the private deletion marker for a branch that a
// receive already removed from the mirror. It runs inside receive callback
// delivery, which holds the receive lock: it must never take that lock again
// (the v0.17.84-v0.17.95 self-deadlock) and never touches the network. A
// branch that exists again on the mirror is left unmarked.
func (c *Cache) MarkBranchDeletion(ctx context.Context, input, branch, oldSHA string) error {
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	if err := ValidateSHA(oldSHA); err != nil {
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
	if !c.isBare(ctx, path) {
		return fmt.Errorf("repository %s is not cached", input)
	}
	return c.markDeletionLocked(ctx, path, branch, oldSHA)
}

func (c *Cache) markDeletionLocked(ctx context.Context, path, branch, oldSHA string) error {
	if _, err := c.capture(ctx, path, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return nil
	}
	if err := c.assertCommit(ctx, path, oldSHA); err != nil {
		// The deleted head was never fetched or has been collected: there
		// is nothing to keep reachable, and propagation still compares by SHA.
		return nil
	}
	return c.updateRef(ctx, path, deletingRefPrefix+branch, oldSHA, "")
}

// BranchHead is one exact branch head selected for deletion.
type BranchHead struct {
	Branch string
	SHA    string
}

// Local prune outcomes.
const (
	PruneDeleted   = "deleted"
	PruneMoved     = "moved"
	PruneMissing   = "missing"
	PruneProtected = "protected"
)

type PruneOutcome struct {
	Branch  string
	SHA     string
	Current string
	Result  string
}

// PruneBranches deletes many exact branch heads from the mirror in one ref
// transaction. Each head is compared and deleted: a moved or missing head is
// reported and left alone. The default and protected branches are refused
// unless allowProtected is set by an explicitly confirmed caller. Only local
// refs change here; upstream propagation is the caller's asynchronous job.
func (c *Cache) PruneBranches(ctx context.Context, input string, heads []BranchHead, allowProtected bool) ([]PruneOutcome, error) {
	if len(heads) == 0 {
		return nil, nil
	}
	for _, head := range heads {
		if err := ValidateBranch(head.Branch); err != nil {
			return nil, err
		}
		if err := ValidateSHA(head.SHA); err != nil {
			return nil, err
		}
	}
	repo, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	receive := c.receiveLock(path)
	if err := receive.LockContext(ctx); err != nil {
		return nil, err
	}
	defer receive.Unlock()
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return nil, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return nil, err
	}
	if !c.isBare(ctx, path) {
		return nil, fmt.Errorf("repository %s is not cached", input)
	}
	if _, err := c.readReservation(c.reservationIdentity(repo, path)); err == nil {
		return nil, ErrReceivePending
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	defaultBranch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("resolve default branch: %w", err)
	}
	current, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/")
	if err != nil {
		return nil, err
	}
	outcomes := make([]PruneOutcome, 0, len(heads))
	var transaction strings.Builder
	transaction.WriteString("start\n")
	deleting := 0
	for _, head := range heads {
		ref := "refs/heads/" + head.Branch
		sha := strings.ToLower(head.SHA)
		outcome := PruneOutcome{Branch: head.Branch, SHA: sha, Current: current[ref]}
		switch {
		case !allowProtected && c.BranchProtected(head.Branch, defaultBranch):
			outcome.Result = PruneProtected
		case outcome.Current == "":
			outcome.Result = PruneMissing
		case outcome.Current != sha:
			outcome.Result = PruneMoved
		default:
			outcome.Result = PruneDeleted
			fmt.Fprintf(&transaction, "delete %s %s\nupdate %s%s %s\n", ref, sha, deletingRefPrefix, head.Branch, sha)
			deleting++
		}
		outcomes = append(outcomes, outcome)
	}
	if deleting == 0 {
		return outcomes, nil
	}
	transaction.WriteString("prepare\ncommit\n")
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"update-ref", "--stdin"}, stdin: strings.NewReader(transaction.String())}); err != nil {
		return nil, fmt.Errorf("delete branches: %w", err)
	}
	return outcomes, nil
}

// BranchInventoryEntry describes one mirror branch for a prune plan.
type BranchInventoryEntry struct {
	Branch      string
	SHA         string
	CommittedAt time.Time
	Merged      bool
	Ahead       int
	Behind      int
	Protected   bool
	Default     bool
}

type BranchInventory struct {
	DefaultBranch string
	Target        string
	TargetSHA     string
	Branches      []BranchInventoryEntry
}

// BranchInventory reads every mirror branch with its relation to target in
// one for-each-ref pass. Read-only.
func (c *Cache) BranchInventory(ctx context.Context, input, target string) (BranchInventory, error) {
	_, path, err := c.path(input)
	if err != nil {
		return BranchInventory{}, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return BranchInventory{}, err
	}
	defer lock.RUnlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return BranchInventory{}, err
	}
	if !c.isBare(ctx, path) {
		return BranchInventory{}, fmt.Errorf("repository %s is not cached", input)
	}
	defaultBranch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return BranchInventory{}, fmt.Errorf("resolve default branch: %w", err)
	}
	if target == "" {
		target = defaultBranch
	}
	if err := ValidateBranch(target); err != nil {
		return BranchInventory{}, err
	}
	targetRef := "refs/heads/" + target
	targetSHA, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", targetRef+"^{commit}")
	if err != nil {
		return BranchInventory{}, fmt.Errorf("branch %s does not exist on the mirror", target)
	}
	inventory := BranchInventory{DefaultBranch: defaultBranch, Target: target, TargetSHA: strings.TrimSpace(targetSHA)}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err = c.run(listCtx, commandSpec{dir: path, args: []string{
		"for-each-ref", fmt.Sprintf("--count=%d", maximumReceiveSnapshotRefs),
		"--format=%(refname) %(objectname) %(committerdate:unix) %(ahead-behind:" + targetRef + ")", "refs/heads/",
	}, stdout: buf})
	if buf.Err() != nil {
		return BranchInventory{}, fmt.Errorf("branch inventory exceeds %d bytes", maxRefsOutput)
	}
	if err != nil {
		return BranchInventory{}, fmt.Errorf("list branches: %w", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 5 || ValidateSHA(fields[1]) != nil {
			return BranchInventory{}, fmt.Errorf("unexpected branch inventory line %q", line)
		}
		branch := strings.TrimPrefix(fields[0], "refs/heads/")
		if ValidateBranch(branch) != nil {
			continue
		}
		seconds, secondsErr := strconv.ParseInt(fields[2], 10, 64)
		ahead, aheadErr := strconv.Atoi(fields[3])
		behind, behindErr := strconv.Atoi(fields[4])
		if secondsErr != nil || aheadErr != nil || behindErr != nil {
			return BranchInventory{}, fmt.Errorf("unexpected branch inventory line %q", line)
		}
		entry := BranchInventoryEntry{
			Branch: branch, SHA: fields[1], CommittedAt: time.Unix(seconds, 0).UTC(),
			Ahead: ahead, Behind: behind, Merged: ahead == 0,
			Default: branch == defaultBranch,
		}
		entry.Protected = c.BranchProtected(branch, defaultBranch)
		inventory.Branches = append(inventory.Branches, entry)
	}
	sort.Slice(inventory.Branches, func(i, j int) bool { return inventory.Branches[i].Branch < inventory.Branches[j].Branch })
	return inventory, nil
}

// BranchPatchEquivalence checks, with git cherry, whether every commit of
// each unmerged head already has an equivalent change in target. At most
// limit heads are checked; the rest are absent from the result. Read-only.
func (c *Cache) BranchPatchEquivalence(ctx context.Context, input, target string, heads []BranchHead, limit int) (map[string]bool, error) {
	if err := ValidateBranch(target); err != nil {
		return nil, err
	}
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
	result := make(map[string]bool, min(len(heads), limit))
	for _, head := range heads {
		if len(result) >= limit || ctx.Err() != nil {
			break
		}
		if ValidateSHA(head.SHA) != nil {
			continue
		}
		output, err := c.capture(ctx, path, "cherry", "refs/heads/"+target, head.SHA)
		if err != nil {
			continue
		}
		result[head.Branch] = patchEquivalent(output)
	}
	return result, nil
}

// patchEquivalent reads git cherry output: "-" marks a commit whose change is
// already in upstream, "+" one that is not.
func patchEquivalent(output string) bool {
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if strings.HasPrefix(line, "+") {
			return false
		}
	}
	return true
}

// UpstreamDeletion is one branch whose upstream ref should go.
type UpstreamDeletion struct {
	Branch string
	OldSHA string
}

// Upstream propagation outcomes.
const (
	UpstreamPropagated = "propagated"
	UpstreamRetry      = "retry"
	UpstreamRefused    = "refused"
	UpstreamSkipped    = "skipped"
)

type UpstreamDeletionResult struct {
	Branch string
	State  string
	Err    error
}

// PropagateBranchDeletions deletes branches on the upstream forge with one
// push per chunk of delete refspecs. No repository lock is held across
// network I/O: the repository lock is taken only for short local reads and
// ref cleanup, and the receive lock is never taken, so a slow forge cannot
// block receives, replay or readiness.
//
// Each upstream ref is compared before deletion: it is removed only while it
// still names the deleted head or one of its ancestors, with a force-with-lease
// on the exact observed SHA. A ref that moved elsewhere is refused. A branch
// that exists again on the mirror is skipped.
func (c *Cache) PropagateBranchDeletions(ctx context.Context, input string, deletions []UpstreamDeletion) ([]UpstreamDeletionResult, error) {
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	results := make([]UpstreamDeletionResult, len(deletions))
	pending := make([]int, 0, len(deletions))
	if err := c.withRepoLock(ctx, path, func() error {
		if err := c.checkRepositoryActive(ctx, input); err != nil {
			return err
		}
		if !c.isBare(ctx, path) {
			return fmt.Errorf("repository %s is not cached", input)
		}
		if err := c.configureRemote(ctx, input, path); err != nil {
			return err
		}
		public, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/")
		if err != nil {
			return err
		}
		for index, deletion := range deletions {
			results[index].Branch = deletion.Branch
			if ValidateBranch(deletion.Branch) != nil {
				results[index].State, results[index].Err = UpstreamRefused, errors.New("branch name is invalid")
				continue
			}
			if public["refs/heads/"+deletion.Branch] != "" {
				results[index].State, results[index].Err = UpstreamSkipped, errors.New("branch exists again on the mirror; upstream left unchanged")
				continue
			}
			if ValidateSHA(deletion.OldSHA) != nil {
				results[index].State, results[index].Err = UpstreamRefused, errors.New("deleted head is unknown; upstream left unchanged")
				continue
			}
			// Replayed pre-upgrade receives have no marker yet.
			if err := c.markDeletionLocked(ctx, path, deletion.Branch, strings.ToLower(deletion.OldSHA)); err != nil {
				return err
			}
			pending = append(pending, index)
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(pending) == 0 {
		return results, c.cleanDeletionRefs(ctx, path, results)
	}
	remote, err := c.remoteHeads(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read upstream branches: %w", err)
	}
	var push []int
	leases := make(map[int]string, len(pending))
	for _, index := range pending {
		deletion := deletions[index]
		observed := remote["refs/heads/"+deletion.Branch]
		old := strings.ToLower(deletion.OldSHA)
		switch {
		case observed == "":
			results[index].State = UpstreamPropagated
		case observed == old || c.localAncestor(ctx, path, observed, old):
			leases[index] = observed
			push = append(push, index)
		default:
			results[index].State = UpstreamRefused
			results[index].Err = fmt.Errorf("upstream %s moved: expected %s or an ancestor, found %s; upstream left unchanged", deletion.Branch, old, observed)
		}
	}
	pushErrs := make(map[int]error, len(push))
	for start := 0; start < len(push); start += upstreamDeletionChunk {
		chunk := push[start:min(start+upstreamDeletionChunk, len(push))]
		args := []string{"push", "--porcelain"}
		for _, index := range chunk {
			args = append(args, "--force-with-lease=refs/heads/"+deletions[index].Branch+":"+leases[index])
		}
		args = append(args, "upstream")
		for _, index := range chunk {
			args = append(args, ":refs/heads/"+deletions[index].Branch)
		}
		if pushErr := c.run(ctx, commandSpec{dir: path, args: args}); pushErr != nil {
			for _, index := range chunk {
				pushErrs[index] = pushErr
			}
		}
	}
	if len(push) > 0 {
		// Read back instead of trusting push output: a lost reply or a
		// partial chunk failure is classified per ref.
		after, readErr := c.remoteHeads(ctx, path)
		for _, index := range push {
			switch {
			case readErr != nil:
				results[index].State, results[index].Err = UpstreamRetry, errors.Join(pushErrs[index], readErr)
			case after["refs/heads/"+deletions[index].Branch] == "":
				results[index].State = UpstreamPropagated
			case pushErrs[index] != nil:
				results[index].State, results[index].Err = UpstreamRetry, pushErrs[index]
			default:
				results[index].State, results[index].Err = UpstreamRetry, errors.New("upstream branch still exists after deletion push")
			}
		}
	}
	return results, c.cleanDeletionRefs(ctx, path, results)
}

// AbandonBranchDeletions removes the markers of deletions that will not
// reach upstream and restores each mirror branch from its upstream tracking
// ref, so the mirror again matches the forge it failed to change.
func (c *Cache) AbandonBranchDeletions(ctx context.Context, input string, branches []string) error {
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	return c.withRepoLock(ctx, path, func() error {
		if !c.isBare(ctx, path) {
			return nil
		}
		for _, branch := range branches {
			if ValidateBranch(branch) != nil {
				continue
			}
			_ = c.deleteRef(ctx, path, deletingRefPrefix+branch, "")
			tracking, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", upstreamRefPrefix+"heads/"+branch)
			if err != nil || ValidateSHA(strings.TrimSpace(tracking)) != nil {
				continue
			}
			// Create only: a branch pushed again in the meantime wins.
			_ = c.updateRef(ctx, path, "refs/heads/"+branch, strings.TrimSpace(tracking), strings.Repeat("0", 40))
		}
		return nil
	})
}

func (c *Cache) cleanDeletionRefs(ctx context.Context, path string, results []UpstreamDeletionResult) error {
	var done []UpstreamDeletionResult
	for _, result := range results {
		if result.State == UpstreamPropagated || result.State == UpstreamSkipped {
			done = append(done, result)
		}
	}
	if len(done) == 0 {
		return nil
	}
	// Local cleanup gets its own short budget: the network phase may have
	// spent the caller's deadline.
	cleanCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	return c.withRepoLock(cleanCtx, path, func() error {
		for _, result := range done {
			_ = c.deleteRef(cleanCtx, path, deletingRefPrefix+result.Branch, "")
			if result.State == UpstreamPropagated {
				// A later refresh must not re-create the branch from a
				// tracking ref fetched before the upstream deletion.
				_ = c.deleteRef(cleanCtx, path, upstreamRefPrefix+"heads/"+result.Branch, "")
			}
		}
		return nil
	})
}

func (c *Cache) withRepoLock(ctx context.Context, path string, operation func() error) error {
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return err
	}
	defer lock.Unlock()
	return operation()
}

func (c *Cache) localAncestor(ctx context.Context, path, older, newer string) bool {
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return false
	}
	defer lock.RUnlock()
	return c.isAncestor(ctx, path, older, newer)
}

// remoteHeads lists upstream branches without any repository lock.
func (c *Cache) remoteHeads(ctx context.Context, path string) (map[string]string, error) {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err := c.run(listCtx, commandSpec{dir: path, args: []string{"ls-remote", "--heads", "upstream"}, stdout: buf})
	if buf.Err() != nil {
		return nil, fmt.Errorf("upstream branch list exceeds %d bytes", maxRefsOutput)
	}
	if err != nil {
		return nil, err
	}
	heads := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || ValidateSHA(fields[0]) != nil || !strings.HasPrefix(fields[1], "refs/heads/") {
			return nil, fmt.Errorf("unexpected upstream branch line %q", line)
		}
		heads[fields[1]] = strings.ToLower(fields[0])
	}
	return heads, nil
}

// deletingBranches lists pending deletion markers by public ref name.
func (c *Cache) deletingBranches(ctx context.Context, path string) (map[string]string, error) {
	markers, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, deletingRefPrefix)
	if err != nil {
		return nil, err
	}
	public := make(map[string]string, len(markers))
	for ref, sha := range markers {
		public["refs/heads/"+strings.TrimPrefix(ref, deletingRefPrefix)] = sha
	}
	return public, nil
}
