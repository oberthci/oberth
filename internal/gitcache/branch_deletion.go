package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"regexp"
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

// reservedBranchesEnv carries the configured reserved-branch globs from the
// server into its receive hooks.
const reservedBranchesEnv = "OBERTH_RESERVED_BRANCHES"

// upstreamDeletionChunk bounds the delete refspecs of one upstream push.
const upstreamDeletionChunk = 100

// ErrReceivePending refuses a prune while an earlier receive of the same
// repository is not yet delivered: its ref diff must not absorb the prune.
var ErrReceivePending = errors.New("an earlier receive of this repository is not yet delivered; retry shortly")

// ReservedBranchPatterns normalizes configured reserved-branch globs.
// Patterns use path.Match syntax; "*" does not cross "/".
func ReservedBranchPatterns(values []string) ([]string, error) {
	patterns := make([]string, 0, len(values))
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		if len(value) > maximumRefPartBytes {
			return nil, fmt.Errorf("reserved branch pattern %q is too long", value)
		}
		if _, err := path.Match(value, ""); err != nil {
			return nil, fmt.Errorf("reserved branch pattern %q: %w", value, err)
		}
		seen[value] = true
		patterns = append(patterns, value)
	}
	return patterns, nil
}

func reservedByPattern(branch string, patterns []string) bool {
	for _, pattern := range patterns {
		if matched, err := path.Match(pattern, branch); err == nil && matched {
			return true
		}
	}
	return false
}

func (c *Cache) reservedByConfig(branch string) bool {
	if c.hookChild {
		patterns, err := ReservedBranchPatterns(strings.Split(os.Getenv(reservedBranchesEnv), ","))
		// An unreadable configuration cannot grant a deletion.
		return err != nil || reservedByPattern(branch, patterns)
	}
	return reservedByPattern(branch, c.reservedBranches)
}

// BranchReserved reports whether a branch is the default branch or matches
// a configured reserved pattern.
func (c *Cache) BranchReserved(branch, defaultBranch string) bool {
	return branch == defaultBranch || c.reservedByConfig(branch)
}

// markReceiveDeletionsLocked runs inside receive finalization, under the
// repository lock, so no refresh can re-create a just-deleted branch before
// its deletion is recorded. A branch created or updated by the same receive
// drops any stale marker: it was pushed again and must not be deleted
// upstream. Marker failures are logged, never fatal: propagation re-creates
// a missing marker.
func (c *Cache) markReceiveDeletionsLocked(ctx context.Context, path string, updates []RefUpdate) {
	for _, update := range updates {
		branch, ok := strings.CutPrefix(update.Ref, "refs/heads/")
		if !ok || ValidateBranch(branch) != nil {
			continue
		}
		var err error
		if update.Deleted() {
			if ValidateSHA(update.OldSHA) == nil {
				err = c.markDeletionLocked(ctx, path, branch, strings.ToLower(update.OldSHA))
			}
		} else {
			err = c.deleteRef(ctx, path, deletingRefPrefix+branch, "")
		}
		if err != nil {
			c.logger.Printf("branch deletion marker for %s: %v", update.Ref, err)
		}
	}
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

// MirrorBranchExists reports whether the mirror still has a public branch.
// A feature-branch publication checks it before force-pushing, so a branch
// deleted after its CI run started is not pushed back upstream.
func (c *Cache) MirrorBranchExists(ctx context.Context, input, branch string) (bool, error) {
	if err := ValidateBranch(branch); err != nil {
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
	if !c.isBare(ctx, path) {
		return false, fmt.Errorf("repository %s is not cached", input)
	}
	refs, err := c.listRefsAtMost(ctx, path, 1, "refs/heads/"+branch)
	if err != nil {
		return false, err
	}
	return refs["refs/heads/"+branch] != "", nil
}

// BranchHead is one exact branch head selected for deletion.
type BranchHead struct {
	Branch string
	SHA    string
}

// Local prune outcomes.
const (
	PruneDeleted  = "deleted"
	PruneMoved    = "moved"
	PruneMissing  = "missing"
	PruneReserved = "reserved"
)

type PruneOutcome struct {
	Branch  string
	SHA     string
	Current string
	Result  string
}

// PruneBranches deletes many exact branch heads from the mirror in one ref
// transaction. Each head is compared and deleted: a moved or missing head is
// reported and left alone. The default branch is always refused; configured
// reserved branches only unless allowReserved is set by an explicitly
// confirmed caller. Only local
// refs change here; upstream propagation is the caller's asynchronous job.
func (c *Cache) PruneBranches(ctx context.Context, input string, heads []BranchHead, allowReserved bool) ([]PruneOutcome, error) {
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
		case head.Branch == defaultBranch || !allowReserved && c.reservedByConfig(head.Branch):
			outcome.Result = PruneReserved
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
	Reserved    bool
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
//
// "Merged" is judged against the forge-confirmed target (the upstream
// tracking ref), never the mirror's own target branch: a fast-forward push
// lands on the mirror before it is published, so while a publication is
// pending or failed the mirror target can hold commits the forge never
// received. A branch merged only into those commits is not merged upstream,
// and deleting it would lose its work there. TargetSHA is the
// forge-confirmed head. A caller that has just verified a published commit may
// supply its exact targetSHA to avoid a lagging upstream tracking ref. This
// does not update any refs; the commit must already exist in the mirror.
func (c *Cache) BranchInventory(ctx context.Context, input, target, targetSHA string) (BranchInventory, error) {
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
	if _, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", "refs/heads/"+target+"^{commit}"); err != nil {
		return BranchInventory{}, fmt.Errorf("branch %s does not exist on the mirror", target)
	}
	if targetSHA == "" {
		targetRef := upstreamRefPrefix + "heads/" + target
		targetSHA, err = c.capture(ctx, path, "rev-parse", "--verify", "--quiet", targetRef+"^{commit}")
		if err != nil {
			return BranchInventory{}, fmt.Errorf("branch %s has no forge-confirmed head on the mirror, so merged branches cannot be judged; publish the default branch to the forge if it has none yet, or wait for the next upstream refresh, then plan again", target)
		}
	}
	targetSHA = strings.TrimSpace(targetSHA)
	if err := ValidateSHA(targetSHA); err != nil {
		return BranchInventory{}, err
	}
	if _, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", targetSHA+"^{commit}"); err != nil {
		return BranchInventory{}, fmt.Errorf("target commit %s is not available on the mirror: %w", targetSHA, err)
	}
	inventory := BranchInventory{DefaultBranch: defaultBranch, Target: target, TargetSHA: targetSHA}
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err = c.run(listCtx, commandSpec{dir: path, args: []string{
		"for-each-ref", fmt.Sprintf("--count=%d", maximumReceiveSnapshotRefs),
		"--format=%(refname) %(objectname) %(committerdate:unix) %(ahead-behind:" + targetSHA + ")", "refs/heads/",
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
		entry.Reserved = c.BranchReserved(branch, defaultBranch)
		inventory.Branches = append(inventory.Branches, entry)
	}
	sort.Slice(inventory.Branches, func(i, j int) bool { return inventory.Branches[i].Branch < inventory.Branches[j].Branch })
	return inventory, nil
}

// BranchPatchEquivalence checks, with git cherry, whether every commit of
// each unmerged head already has an equivalent change in targetSHA. At most
// limit heads are checked; the rest are absent from the result. Read-only and
// lock-free: every input is an exact object ID from an earlier snapshot, so
// concurrent ref changes cannot affect the answer, and receives never wait
// behind a long plan. Execution still compares and deletes each head.
func (c *Cache) BranchPatchEquivalence(ctx context.Context, input, targetSHA string, heads []BranchHead, limit int) (map[string]bool, error) {
	if err := ValidateSHA(targetSHA); err != nil {
		return nil, err
	}
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	result := make(map[string]bool, min(len(heads), limit))
	for _, head := range heads {
		if len(result) >= limit || ctx.Err() != nil {
			break
		}
		if ValidateSHA(head.SHA) != nil {
			continue
		}
		output, err := c.capture(ctx, path, "cherry", targetSHA, head.SHA)
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
	// UpstreamArchived is final: the forge refuses every push because the
	// repository is archived. The mirror deletion and its marker are kept,
	// so a refresh does not re-create the branch from the read-only forge.
	UpstreamArchived = "archived"
)

// archivedUpstreamMessage matches the push refusal of a read-only archived
// repository: Forgejo and Gitea print "Repo ... is archived", GitHub "This
// repository was archived so it is read-only". Only a refused push is
// classified, after the read-back shows the ref unchanged.
var archivedUpstreamMessage = regexp.MustCompile(`(?i)\brepo\b[^\n]{0,300}\bis archived\b|\brepository was archived so it is read-only\b`)

// ArchivedUpstream reports whether a push failure says the upstream
// repository is archived.
func ArchivedUpstream(err error) bool {
	return err != nil && archivedUpstreamMessage.MatchString(err.Error())
}

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
// still names a deleted head or one of its ancestors, with a force-with-lease
// on the exact observed SHA. A ref that moved elsewhere is refused. A push the
// forge refuses because the repository is archived is final (UpstreamArchived)
// and keeps the deletion marker. A branch
// that exists again on the mirror without a deletion marker was pushed again
// and is skipped; with a marker its local deletion has not been applied yet,
// so it is retried later. Several records for one branch share one refspec
// and one outcome.
func (c *Cache) PropagateBranchDeletions(ctx context.Context, input string, deletions []UpstreamDeletion) ([]UpstreamDeletionResult, error) {
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	results := make([]UpstreamDeletionResult, len(deletions))
	pending := make(map[string][]int)
	var order []string
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
		markers, err := c.deletingBranches(ctx, path)
		if err != nil {
			return err
		}
		for index, deletion := range deletions {
			results[index].Branch = deletion.Branch
			ref := "refs/heads/" + deletion.Branch
			switch {
			case ValidateBranch(deletion.Branch) != nil:
				results[index].State, results[index].Err = UpstreamRefused, errors.New("branch name is invalid")
			case public[ref] != "" && markers[ref] != "":
				results[index].State, results[index].Err = UpstreamRetry, errors.New("local deletion not applied yet; upstream left unchanged")
			case public[ref] != "":
				results[index].State, results[index].Err = UpstreamSkipped, errors.New("branch exists again on the mirror; upstream left unchanged")
			case ValidateSHA(deletion.OldSHA) != nil:
				results[index].State, results[index].Err = UpstreamRefused, errors.New("deleted head is unknown; upstream left unchanged")
			default:
				// Replayed pre-upgrade receives have no marker yet.
				if markers[ref] == "" {
					if err := c.markDeletionLocked(ctx, path, deletion.Branch, strings.ToLower(deletion.OldSHA)); err != nil {
						return err
					}
				}
				if _, seen := pending[deletion.Branch]; !seen {
					order = append(order, deletion.Branch)
				}
				pending[deletion.Branch] = append(pending[deletion.Branch], index)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}
	if len(order) == 0 {
		return results, c.cleanDeletionRefs(ctx, path, results)
	}
	remote, err := c.remoteHeads(ctx, path)
	if err != nil {
		return nil, fmt.Errorf("read upstream branches: %w", err)
	}
	setAll := func(branch, state string, err error) {
		for _, index := range pending[branch] {
			results[index].State, results[index].Err = state, err
		}
	}
	var push []string
	leases := make(map[string]string, len(order))
	for _, branch := range order {
		observed := remote["refs/heads/"+branch]
		if observed == "" {
			setAll(branch, UpstreamPropagated, nil)
			continue
		}
		eligible := false
		var expected []string
		for _, index := range pending[branch] {
			old := strings.ToLower(deletions[index].OldSHA)
			expected = append(expected, old)
			if observed == old || c.localAncestor(ctx, path, observed, old) {
				eligible = true
				break
			}
		}
		if !eligible {
			setAll(branch, UpstreamRefused, fmt.Errorf("upstream %s moved: expected %s or an ancestor, found %s; upstream left unchanged", branch, strings.Join(expected, " or "), observed))
			continue
		}
		leases[branch] = observed
		push = append(push, branch)
	}
	pushErrs := make(map[string]error, len(push))
	for start := 0; start < len(push); start += upstreamDeletionChunk {
		chunk := push[start:min(start+upstreamDeletionChunk, len(push))]
		args := []string{"push", "--porcelain"}
		for _, branch := range chunk {
			args = append(args, "--force-with-lease=refs/heads/"+branch+":"+leases[branch])
		}
		args = append(args, "upstream")
		for _, branch := range chunk {
			args = append(args, ":refs/heads/"+branch)
		}
		if pushErr := c.run(ctx, commandSpec{dir: path, args: args}); pushErr != nil {
			for _, branch := range chunk {
				pushErrs[branch] = pushErr
			}
		}
	}
	if len(push) > 0 {
		// Read back instead of trusting push output: a lost reply or a
		// partial chunk failure is classified per ref.
		after, readErr := c.remoteHeads(ctx, path)
		for _, branch := range push {
			switch {
			case readErr != nil:
				setAll(branch, UpstreamRetry, errors.Join(pushErrs[branch], readErr))
			case after["refs/heads/"+branch] == "":
				setAll(branch, UpstreamPropagated, nil)
			case ArchivedUpstream(pushErrs[branch]):
				setAll(branch, UpstreamArchived, pushErrs[branch])
			case pushErrs[branch] != nil:
				setAll(branch, UpstreamRetry, pushErrs[branch])
			default:
				setAll(branch, UpstreamRetry, errors.New("upstream branch still exists after deletion push"))
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
