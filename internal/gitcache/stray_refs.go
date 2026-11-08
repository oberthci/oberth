package gitcache

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/boundedio"
)

// Stray refs (#1504) are refs outside the public refs/heads/ and refs/tags/
// namespaces and outside Oberth's own private namespaces, for example
// refs/remotes/upstream/* left by an old mirror tool. They are never fetched,
// received or mirrored, so nothing else removes them, and they keep old
// commits reachable. Only an explicit admin plan deletes them, locally, with
// a compare-and-delete lease on each exact object ID.

// MaximumStrayRefs bounds one stray-ref inventory.
const MaximumStrayRefs = 5000

// strayRefProtected lists the namespaces a stray-ref operation never lists or
// changes: public branches and tags, Oberth's private state, and replacement
// refs (hidden and removed by Oberth itself).
var strayRefProtected = []string{"refs/heads/", "refs/tags/", "refs/oberth/", "refs/replace/"}

// StrayRefProtected reports whether ref or prefix lies in a namespace that a
// stray-ref operation must never touch.
func StrayRefProtected(ref string) bool {
	for _, namespace := range strayRefProtected {
		if strings.HasPrefix(ref, namespace) || ref+"/" == namespace {
			return true
		}
	}
	return false
}

// ValidateStrayRef accepts a full refs/... name outside the protected
// namespaces whose remainder is a valid ref path.
func ValidateStrayRef(ref string) error {
	rest, ok := strings.CutPrefix(ref, "refs/")
	if !ok || StrayRefProtected(ref) {
		return fmt.Errorf("ref %q is not a stray ref", ref)
	}
	for _, char := range ref {
		if char < 0x20 || char == 0x7f {
			return fmt.Errorf("invalid ref name %q", ref)
		}
	}
	return validateRefPart("ref", rest)
}

// StrayRef is one stray ref with its relation to the default branch and tags.
type StrayRef struct {
	Ref string
	// SHA is the object the ref names; a symbolic ref reports its target's.
	SHA        string
	ObjectType string
	// Commit is the commit the ref peels to, empty when it peels to none.
	Commit      string
	Symref      string
	CommittedAt time.Time
	// Reachability of Commit; both false for a ref that peels to no commit.
	ReachableFromDefault bool
	ReachableFromTag     bool
}

type StrayRefInventory struct {
	DefaultBranch string
	DefaultSHA    string
	Refs          []StrayRef
	// Truncated reports more than MaximumStrayRefs stray refs; Refs holds
	// the first ones by name.
	Truncated bool
}

// StrayRefInventory lists the stray refs of a cached repository, sorted by
// name, and classifies each target as reachable from the default branch,
// from a tag, or neither. Read-only. The listing is one snapshot under the
// read lock; the reachability walk runs lock-free on exact object IDs, like
// BranchPatchEquivalence.
func (c *Cache) StrayRefInventory(ctx context.Context, input string) (StrayRefInventory, error) {
	_, path, err := c.path(input)
	if err != nil {
		return StrayRefInventory{}, err
	}
	var inventory StrayRefInventory
	var tags map[string]string
	if err := func() error {
		lock := c.repoLock(path)
		if err := lock.RLockContext(ctx); err != nil {
			return err
		}
		defer lock.RUnlock()
		if err := c.checkRepositoryActive(ctx, input); err != nil {
			return err
		}
		if !c.isBare(ctx, path) {
			return fmt.Errorf("repository %s is not cached", input)
		}
		defaultBranch, err := c.currentDefaultBranch(ctx, path)
		if err != nil {
			return fmt.Errorf("resolve default branch: %w", err)
		}
		defaultSHA, err := c.capture(ctx, path, "rev-parse", "--verify", "--quiet", "refs/heads/"+defaultBranch+"^{commit}")
		if err != nil {
			return fmt.Errorf("branch %s does not exist on the mirror", defaultBranch)
		}
		inventory.DefaultBranch, inventory.DefaultSHA = defaultBranch, strings.TrimSpace(defaultSHA)
		inventory.Refs, inventory.Truncated, err = c.listStrayRefs(ctx, path)
		if err != nil {
			return err
		}
		tags, err = c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/tags/")
		return err
	}(); err != nil {
		return StrayRefInventory{}, err
	}
	commits := make([]string, 0, len(inventory.Refs))
	seen := make(map[string]bool, len(inventory.Refs))
	for _, ref := range inventory.Refs {
		if ref.Commit != "" && !seen[ref.Commit] {
			seen[ref.Commit] = true
			commits = append(commits, ref.Commit)
		}
	}
	if len(commits) == 0 {
		return inventory, nil
	}
	// A commit is reachable from the default branch unless a walk from it
	// that excludes the default branch reaches it; likewise for tags.
	outsideDefault, err := c.commitsOutside(ctx, path, commits, []string{inventory.DefaultSHA})
	if err != nil {
		return StrayRefInventory{}, fmt.Errorf("walk stray refs against %s: %w", inventory.DefaultBranch, err)
	}
	var remaining []string
	for _, commit := range commits {
		if outsideDefault[commit] {
			remaining = append(remaining, commit)
		}
	}
	outsideTags := map[string]bool{}
	if len(remaining) > 0 {
		exclude := make([]string, 0, len(tags)+1)
		exclude = append(exclude, inventory.DefaultSHA)
		for _, sha := range tags {
			exclude = append(exclude, sha)
		}
		sort.Strings(exclude[1:])
		outsideTags, err = c.commitsOutside(ctx, path, remaining, exclude)
		if err != nil {
			return StrayRefInventory{}, fmt.Errorf("walk stray refs against tags: %w", err)
		}
	}
	for index := range inventory.Refs {
		ref := &inventory.Refs[index]
		if ref.Commit == "" {
			continue
		}
		ref.ReachableFromDefault = !outsideDefault[ref.Commit]
		ref.ReachableFromTag = outsideDefault[ref.Commit] && !outsideTags[ref.Commit]
	}
	return inventory, nil
}

// listStrayRefs reads every stray ref, at most MaximumStrayRefs, by name.
func (c *Cache) listStrayRefs(ctx context.Context, path string) ([]StrayRef, bool, error) {
	listCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"for-each-ref", fmt.Sprintf("--count=%d", MaximumStrayRefs+1),
		"--format=%(refname)%00%(objectname)%00%(objecttype)%00%(*objectname)%00%(*objecttype)%00%(symref)%00%(committerdate:unix)%00%(*committerdate:unix)"}
	for _, namespace := range strayRefProtected {
		args = append(args, "--exclude="+strings.TrimSuffix(namespace, "/"))
	}
	args = append(args, "refs/")
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err := c.run(listCtx, commandSpec{dir: path, args: args, stdout: buf})
	if buf.Err() != nil {
		return nil, false, fmt.Errorf("stray ref list exceeds %d bytes", maxRefsOutput)
	}
	if err != nil {
		return nil, false, fmt.Errorf("list stray refs: %w", err)
	}
	var refs []StrayRef
	truncated := false
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		fields := strings.Split(line, "\x00")
		if len(fields) != 8 || ValidateSHA(fields[1]) != nil {
			return nil, false, fmt.Errorf("unexpected stray ref line %q", line)
		}
		// Belt and braces: the --exclude patterns already dropped these.
		if ValidateStrayRef(fields[0]) != nil {
			continue
		}
		if len(refs) == MaximumStrayRefs {
			truncated = true
			break
		}
		ref := StrayRef{Ref: fields[0], SHA: fields[1], ObjectType: fields[2], Symref: fields[5]}
		seconds := fields[6]
		switch {
		case fields[2] == "commit":
			ref.Commit = fields[1]
		case fields[2] == "tag" && fields[4] == "commit" && ValidateSHA(fields[3]) == nil:
			ref.Commit, seconds = fields[3], fields[7]
		}
		if ref.Commit != "" {
			if value, err := strconv.ParseInt(seconds, 10, 64); err == nil {
				ref.CommittedAt = time.Unix(value, 0).UTC()
			}
		}
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Ref < refs[j].Ref })
	return refs, truncated, nil
}

// commitsOutside walks from commits, excluding everything reachable from
// exclude, and reports which of the commits the walk reaches: exactly those
// not reachable from exclude. The walk output is streamed, never buffered.
func (c *Cache) commitsOutside(ctx context.Context, path string, commits, exclude []string) (map[string]bool, error) {
	var input strings.Builder
	for _, commit := range commits {
		input.WriteString(commit + "\n")
	}
	input.WriteString("--not\n")
	for _, sha := range exclude {
		input.WriteString(sha + "\n")
	}
	wanted := make(map[string]bool, len(commits))
	for _, commit := range commits {
		wanted[commit] = true
	}
	matcher := &commitMatcher{wanted: wanted, found: make(map[string]bool, len(commits))}
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"rev-list", "--stdin"}, stdin: strings.NewReader(input.String()), stdout: matcher}); err != nil {
		return nil, err
	}
	matcher.flush()
	return matcher.found, nil
}

// commitMatcher consumes rev-list output line by line and keeps only the
// wanted object IDs, so a long walk costs no memory beyond one line.
type commitMatcher struct {
	wanted  map[string]bool
	found   map[string]bool
	partial []byte
}

func (m *commitMatcher) Write(data []byte) (int, error) {
	written := len(data)
	for len(data) > 0 {
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			if len(m.partial)+len(data) > 128 {
				return 0, fmt.Errorf("unexpected rev-list output")
			}
			m.partial = append(m.partial, data...)
			break
		}
		m.match(append(m.partial, data[:newline]...))
		m.partial = m.partial[:0]
		data = data[newline+1:]
	}
	return written, nil
}

func (m *commitMatcher) match(line []byte) {
	if sha := strings.TrimSpace(string(line)); m.wanted[sha] {
		m.found[sha] = true
	}
}

func (m *commitMatcher) flush() {
	if len(m.partial) > 0 {
		m.match(m.partial)
		m.partial = m.partial[:0]
	}
}

// StrayRefHead is one exact stray ref selected for deletion.
type StrayRefHead struct {
	Ref string
	SHA string
}

// Stray ref prune outcomes reuse PruneDeleted, PruneMoved and PruneMissing.
type StrayRefOutcome struct {
	Ref     string
	SHA     string
	Current string
	Result  string
}

// PruneStrayRefs deletes exact stray refs from the mirror in one ref
// transaction under the repository lock. Each ref is compared with its
// expected object ID and deleted with that ID as the lease; a moved or
// missing ref is reported and left alone. A symbolic ref is deleted itself,
// never its target. Public branches and tags and Oberth's private refs are
// refused before any Git runs. Stray refs are never mirrored, so nothing is
// propagated upstream.
func (c *Cache) PruneStrayRefs(ctx context.Context, input string, heads []StrayRefHead) ([]StrayRefOutcome, error) {
	if len(heads) == 0 {
		return nil, nil
	}
	if len(heads) > MaximumStrayRefs {
		return nil, fmt.Errorf("at most %d stray refs per deletion", MaximumStrayRefs)
	}
	seen := make(map[string]bool, len(heads))
	for _, head := range heads {
		if err := ValidateStrayRef(head.Ref); err != nil {
			return nil, err
		}
		if err := ValidateSHA(head.SHA); err != nil {
			return nil, err
		}
		if seen[head.Ref] {
			return nil, fmt.Errorf("stray ref %s is listed twice", head.Ref)
		}
		seen[head.Ref] = true
	}
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
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
	listed, _, err := c.listStrayRefs(ctx, path)
	if err != nil {
		return nil, err
	}
	current := make(map[string]string, len(listed))
	for _, ref := range listed {
		current[ref.Ref] = ref.SHA
	}
	outcomes := make([]StrayRefOutcome, 0, len(heads))
	var transaction strings.Builder
	transaction.WriteString("start\n")
	deleting := 0
	for _, head := range heads {
		sha := strings.ToLower(head.SHA)
		outcome := StrayRefOutcome{Ref: head.Ref, SHA: sha, Current: current[head.Ref]}
		switch {
		case outcome.Current == "":
			outcome.Result = PruneMissing
		case outcome.Current != sha:
			outcome.Result = PruneMoved
		default:
			outcome.Result = PruneDeleted
			fmt.Fprintf(&transaction, "delete %s %s\n", head.Ref, sha)
			deleting++
		}
		outcomes = append(outcomes, outcome)
	}
	if deleting == 0 {
		return outcomes, nil
	}
	transaction.WriteString("prepare\ncommit\n")
	// --no-deref: a symbolic ref such as refs/remotes/upstream/HEAD is
	// removed itself; its target is never followed.
	if err := c.run(ctx, commandSpec{dir: path, args: []string{"update-ref", "--no-deref", "--stdin"}, stdin: strings.NewReader(transaction.String())}); err != nil {
		return nil, fmt.Errorf("delete stray refs: %w", err)
	}
	return outcomes, nil
}
