package gitcache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const upstreamRefPrefix = "refs/oberth/upstream/"
const releaseAdmissionRefPrefix = "refs/oberth/release-admission/heads/"
const receiveOutboxDirectory = ".receive-outbox"

var receiveUpdateHook = strings.Replace(receiveUpdateHookTemplate, "__MAX_REF_PART_BYTES__", fmt.Sprint(maximumRefPartBytes), 1)

const receiveUpdateHookTemplate = `#!/bin/sh
set -eu
LC_ALL=C
export LC_ALL

ref=$1
old=$2
new=$3

is_zero_oid() {
  case "$1" in
    ''|*[!0]*) return 1 ;;
    *) return 0 ;;
  esac
}

reject() {
  printf 'oberth: rejected %s update: %s\n' "$1" "$2" >&2
  exit 1
}

validate_public_name() {
  kind=$1
  name=$2
  [ -n "$name" ] || reject "$kind" "$kind name has invalid length"
  [ "${#name}" -le __MAX_REF_PART_BYTES__ ] || reject "$kind" "$kind name has invalid length"
  case "$name" in
    -*|/*|refs/*|*/) reject "$kind" "invalid $kind name" ;;
  esac
}

case "$ref" in
  refs/heads/*)
    branch=${ref#refs/heads/}
    validate_public_name branch "$branch"
    ;;
  refs/tags/*)
    tag=${ref#refs/tags/}
    validate_public_name tag "$tag"
    [ "$old" = "$new" ] && exit 0
    is_zero_oid "$old" || reject tag "release tags are immutable"
    is_zero_oid "$new" && reject tag "release tags cannot be deleted"
    upstream_ref=refs/oberth/upstream/tags/$tag
    if git show-ref --verify --quiet "$upstream_ref"; then
      reject tag "tag already exists upstream"
    fi
    default_ref=$(git symbolic-ref --quiet HEAD) || reject tag "default branch is unavailable"
    case "$default_ref" in
      refs/heads/*) default_branch=${default_ref#refs/heads/} ;;
      *) reject tag "default branch is invalid" ;;
    esac
    validate_public_name branch "$default_branch"
    admission_ref=refs/oberth/release-admission/heads/$default_branch
    commit=$(git rev-parse --verify "$new^{commit}") || reject tag "tag does not select a commit"
    git merge-base --is-ancestor "$commit" "$admission_ref" || reject tag "tag commit is not reachable from the fresh upstream default branch"
    ;;
esac
`

// Cache owns bare repositories under one persistent root.
type Cache struct {
	root         string
	outboxRoot   string
	upstream     func(string) (string, error)
	gitBinary    string
	timeout      time.Duration
	env          map[string]string
	redact       []string
	logger       Logger
	locks        sync.Map
	receiveLocks sync.Map
}

// New validates static configuration without contacting an upstream.
func New(config Config) (*Cache, error) {
	if config.Root == "" {
		return nil, errors.New("git cache root is required")
	}
	if config.Upstream == nil {
		return nil, errors.New("upstream resolver is required")
	}
	root, err := filepath.Abs(config.Root)
	if err != nil {
		return nil, fmt.Errorf("resolve git cache root: %w", err)
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, fmt.Errorf("create git cache root: %w", err)
	}
	outboxRoot := filepath.Join(root, receiveOutboxDirectory)
	if err := os.MkdirAll(outboxRoot, 0o700); err != nil {
		return nil, fmt.Errorf("create receive outbox: %w", err)
	}
	gitBinary := config.GitBinary
	if gitBinary == "" {
		gitBinary = "git"
	}
	logger := config.Logger
	if logger == nil {
		logger = discardLogger{}
	}
	for key := range config.Env {
		if key == "" || strings.Contains(key, "=") {
			return nil, fmt.Errorf("invalid Git environment key %q", key)
		}
	}
	return &Cache{
		root:       root,
		outboxRoot: outboxRoot,
		upstream:   config.Upstream,
		gitBinary:  gitBinary,
		timeout:    defaultTimeout(config.CommandTimeout),
		env:        cloneMap(config.Env),
		redact:     append([]string(nil), config.Redact...),
		logger:     logger,
	}, nil
}

// Ensure creates or refreshes one cache. A refresh failure is non-fatal only
// after a valid bare repository already exists.
func (c *Cache) Ensure(ctx context.Context, input string) (Repository, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return Repository{}, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return c.ensureLocked(ctx, repo, path)
}

func (c *Cache) ensureLocked(ctx context.Context, repo, path string) (Repository, error) {
	if stat, err := os.Stat(path); err == nil {
		if !stat.IsDir() || !c.isBare(ctx, path) {
			return Repository{}, fmt.Errorf("cache path for %s is not a bare repository", repo)
		}
		if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
			return Repository{}, fmt.Errorf("clean replacement refs for %s: %w", repo, err)
		}
		if err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
			return Repository{}, fmt.Errorf("clean invalid public refs for %s: %w", repo, err)
		}
		if err := installReceiveUpdateHook(path); err != nil {
			return Repository{}, fmt.Errorf("install receive policy for %s: %w", repo, err)
		}
		branch, refreshErr := c.refresh(ctx, repo, path)
		if refreshErr != nil {
			c.logger.Printf("upstream refresh unavailable for %s; serving stale cache", repo)
			branch, err = c.currentDefaultBranch(ctx, path)
			if err != nil {
				return Repository{}, errors.Join(refreshErr, fmt.Errorf("read cached default branch: %w", err))
			}
			return Repository{Path: path, Stale: true, DefaultBranch: branch}, nil
		}
		return Repository{Path: path, DefaultBranch: branch}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Repository{}, fmt.Errorf("inspect cache for %s: %w", repo, err)
	}

	temporary, err := os.MkdirTemp(c.root, "."+repo+"-init-")
	if err != nil {
		return Repository{}, fmt.Errorf("create temporary cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	if err := c.run(ctx, commandSpec{args: []string{"init", "--bare", temporary}}); err != nil {
		return Repository{}, err
	}
	if err := c.configureRemote(ctx, repo, temporary); err != nil {
		return Repository{}, err
	}
	if err := c.fetchTracking(ctx, temporary); err != nil {
		return Repository{}, fmt.Errorf("initial upstream fetch for %s: %w", repo, err)
	}
	if err := c.materialize(ctx, temporary, nil); err != nil {
		return Repository{}, err
	}
	branch, err := c.discoverDefaultBranch(ctx, temporary)
	if err != nil {
		return Repository{}, fmt.Errorf("discover default branch for %s: %w", repo, err)
	}
	if err := c.setHead(ctx, temporary, branch); err != nil {
		return Repository{}, err
	}
	if err := installReceiveUpdateHook(temporary); err != nil {
		return Repository{}, fmt.Errorf("install receive policy for %s: %w", repo, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return Repository{}, fmt.Errorf("publish cache for %s: %w", repo, err)
	}
	return Repository{Path: path, DefaultBranch: branch}, nil
}

func installReceiveUpdateHook(repository string) error {
	hooks := filepath.Join(repository, "hooks")
	info, err := os.Lstat(hooks)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("git hooks path is not a directory")
	}
	temporary, err := os.CreateTemp(hooks, ".oberth-update-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.WriteString(temporary, receiveUpdateHook); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// #nosec G302 -- Git hooks must be executable; owner-only mode is deliberate.
	if err := os.Chmod(temporaryPath, 0o700); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, filepath.Join(hooks, "update")); err != nil {
		return err
	}
	return syncDirectory(hooks)
}

func (c *Cache) prepareReleaseAdmissionLocked(ctx context.Context, path string, enabled bool) (ReleaseAdmission, error) {
	defaultBranch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return ReleaseAdmission{}, fmt.Errorf("resolve cached default branch: %w", err)
	}
	admissionRef := releaseAdmissionRefPrefix + defaultBranch
	if !enabled {
		return ReleaseAdmission{}, c.deleteRef(ctx, path, admissionRef, "")
	}
	upstreamRef := upstreamRefPrefix + "heads/" + defaultBranch
	sha, err := c.capture(ctx, path, "rev-parse", "--verify", upstreamRef)
	if err != nil {
		return ReleaseAdmission{}, fmt.Errorf("resolve fresh upstream default branch: %w", err)
	}
	sha = strings.TrimSpace(sha)
	if err := ValidateSHA(sha); err != nil {
		return ReleaseAdmission{}, err
	}
	if err := c.updateRef(ctx, path, admissionRef, sha, ""); err != nil {
		return ReleaseAdmission{}, err
	}
	return ReleaseAdmission{DefaultBranch: defaultBranch, SHA: sha}, nil
}

func (c *Cache) refresh(ctx context.Context, repo, path string) (string, error) {
	old, err := c.listRefs(ctx, path, upstreamRefPrefix)
	if err != nil {
		return "", err
	}
	if err := c.configureRemote(ctx, repo, path); err != nil {
		return "", err
	}
	if err := c.fetchTracking(ctx, path); err != nil {
		return "", err
	}
	if err := c.materialize(ctx, path, old); err != nil {
		return "", err
	}
	branch, err := c.discoverDefaultBranch(ctx, path)
	if err != nil {
		return "", err
	}
	if err := c.setHead(ctx, path, branch); err != nil {
		return "", err
	}
	return branch, nil
}

func (c *Cache) configureRemote(ctx context.Context, repo, path string) error {
	remote, err := c.upstream(repo)
	if err != nil {
		return fmt.Errorf("resolve upstream for %s: %w", repo, err)
	}
	if strings.TrimSpace(remote) == "" {
		return fmt.Errorf("upstream for %s is empty", repo)
	}
	if err := ValidateUpstream(remote); err != nil {
		return fmt.Errorf("upstream for %s: %w", repo, err)
	}
	if _, err := c.capture(ctx, path, "remote", "get-url", "upstream"); err != nil {
		return c.run(ctx, commandSpec{dir: path, args: []string{"remote", "add", "upstream", remote}})
	}
	return c.run(ctx, commandSpec{dir: path, args: []string{"remote", "set-url", "upstream", remote}})
}

func (c *Cache) fetchTracking(ctx context.Context, path string) error {
	return c.run(ctx, commandSpec{dir: path, args: []string{
		"fetch", "--prune", "--no-tags", "upstream",
		"+refs/heads/*:" + upstreamRefPrefix + "heads/*",
		"+refs/tags/*:" + upstreamRefPrefix + "tags/*",
	}})
}

func (c *Cache) materialize(ctx context.Context, path string, old map[string]string) error {
	current, err := c.listRefs(ctx, path, upstreamRefPrefix)
	if err != nil {
		return err
	}
	public, err := c.listRefs(ctx, path, "refs/heads/", "refs/tags/")
	if err != nil {
		return err
	}
	for tracking, newSHA := range current {
		ref, ok := publicRef(tracking)
		if !ok {
			continue
		}
		oldTracking, existed := old[tracking]
		publicSHA, publicExists := public[ref]
		if publicExists && (!existed || publicSHA != oldTracking) {
			continue
		}
		if err := c.updateRef(ctx, path, ref, newSHA, publicSHA); err != nil {
			return err
		}
	}
	for tracking, oldSHA := range old {
		if _, exists := current[tracking]; exists {
			continue
		}
		ref, ok := publicRef(tracking)
		if !ok || public[ref] != oldSHA {
			continue
		}
		if err := c.deleteRef(ctx, path, ref, oldSHA); err != nil {
			return err
		}
	}
	return nil
}

func (c *Cache) cleanInvalidPublicRefsLocked(ctx context.Context, path string) error {
	public, err := c.listRefs(ctx, path, "refs/heads/", "refs/tags/")
	if err != nil {
		return err
	}
	refs := make([]string, 0, len(public))
	for ref := range public {
		if !publicReceiveRef(ref) {
			refs = append(refs, ref)
		}
	}
	sort.Strings(refs)
	for _, ref := range refs {
		if err := c.deleteRef(ctx, path, ref, public[ref]); err != nil {
			return fmt.Errorf("delete invalid public ref %s: %w", ref, err)
		}
	}
	return nil
}

func publicRef(tracking string) (string, bool) {
	switch {
	case strings.HasPrefix(tracking, upstreamRefPrefix+"heads/"):
		branch := strings.TrimPrefix(tracking, upstreamRefPrefix+"heads/")
		if ValidateBranch(branch) != nil {
			return "", false
		}
		return "refs/heads/" + branch, true
	case strings.HasPrefix(tracking, upstreamRefPrefix+"tags/"):
		tag := strings.TrimPrefix(tracking, upstreamRefPrefix+"tags/")
		if ValidateTag(tag) != nil {
			return "", false
		}
		return "refs/tags/" + tag, true
	default:
		return "", false
	}
}

func (c *Cache) updateRef(ctx context.Context, path, ref, newSHA, oldSHA string) error {
	args := []string{"update-ref", ref, newSHA}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	return c.run(ctx, commandSpec{dir: path, args: args})
}

func (c *Cache) deleteRef(ctx context.Context, path, ref, oldSHA string) error {
	args := []string{"update-ref", "-d", ref}
	if oldSHA != "" {
		args = append(args, oldSHA)
	}
	return c.run(ctx, commandSpec{dir: path, args: args})
}

func (c *Cache) discoverDefaultBranch(ctx context.Context, path string) (string, error) {
	output, err := c.capture(ctx, path, "ls-remote", "--symref", "upstream", "HEAD")
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "ref:" || fields[2] != "HEAD" || !strings.HasPrefix(fields[1], "refs/heads/") {
			continue
		}
		branch := strings.TrimPrefix(fields[1], "refs/heads/")
		if err := ValidateBranch(branch); err != nil {
			return "", fmt.Errorf("invalid upstream default branch: %w", err)
		}
		return branch, nil
	}
	return "", errors.New("upstream did not advertise a symbolic default branch")
}

func (c *Cache) currentDefaultBranch(ctx context.Context, path string) (string, error) {
	output, err := c.capture(ctx, path, "symbolic-ref", "HEAD")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(output, "refs/heads/") {
		return "", errors.New("cached HEAD is not a branch")
	}
	branch := strings.TrimPrefix(output, "refs/heads/")
	if err := ValidateBranch(branch); err != nil {
		return "", err
	}
	return branch, nil
}

func (c *Cache) setHead(ctx context.Context, path, branch string) error {
	if err := ValidateBranch(branch); err != nil {
		return err
	}
	ref := "refs/heads/" + branch
	if _, err := c.capture(ctx, path, "show-ref", "--verify", "--quiet", ref); err != nil {
		return fmt.Errorf("upstream default branch %s was not fetched: %w", branch, err)
	}
	return c.run(ctx, commandSpec{dir: path, args: []string{"symbolic-ref", "HEAD", ref}})
}

func (c *Cache) isBare(ctx context.Context, path string) bool {
	output, err := c.capture(ctx, path, "rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(output) == "true"
}

func (c *Cache) path(input string) (string, string, error) {
	repo, err := NormalizeRepo(input)
	if err != nil {
		return "", "", err
	}
	path := filepath.Join(c.root, repo+".git")
	relative, err := filepath.Rel(c.root, path)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("repository path escapes cache root")
	}
	return repo, path, nil
}

func (c *Cache) repoLock(repo string) *sync.Mutex {
	value, _ := c.locks.LoadOrStore(repo, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// receiveLock serializes complete receive lifecycles, including durable
// callback delivery, without forcing callbacks to run under repoLock. Receive
// handlers need to inspect the accepted Git objects and therefore legitimately
// re-enter Cache methods that acquire repoLock themselves.
func (c *Cache) receiveLock(repo string) *sync.Mutex {
	value, _ := c.receiveLocks.LoadOrStore(repo, &sync.Mutex{})
	return value.(*sync.Mutex)
}

// RefSHA resolves a branch to its commit SHA in the cached bare repository
// without contacting the upstream. Returns an error if the cache does not
// exist or the branch is not present.
func (c *Cache) RefSHA(ctx context.Context, input string, branch string) (string, error) {
	if err := ValidateBranch(branch); err != nil {
		return "", err
	}
	repo, path, err := c.path(input)
	if err != nil {
		return "", err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return "", fmt.Errorf("repository %s is not cached", repo)
	}
	output, err := c.capture(ctx, path, "rev-parse", "--verify", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %s not found in %s", branch, repo)
	}
	sha := strings.TrimSpace(output)
	if err := ValidateSHA(sha); err != nil {
		return "", err
	}
	return sha, nil
}

// SnapshotRefs returns only client-owned public branch and tag refs.
func (c *Cache) SnapshotRefs(ctx context.Context, input string) (map[string]string, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return c.listRefs(ctx, path, "refs/heads/", "refs/tags/")
}

func (c *Cache) cleanReplacementRefsLocked(ctx context.Context, path string) error {
	refs, err := c.listRefs(ctx, path, "refs/replace/")
	if err != nil {
		return err
	}
	for ref, sha := range refs {
		if err := c.deleteRef(ctx, path, ref, sha); err != nil {
			return fmt.Errorf("delete replacement ref %s: %w", ref, err)
		}
	}
	return nil
}

func (c *Cache) listRefs(ctx context.Context, path string, prefixes ...string) (map[string]string, error) {
	args := append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, prefixes...)
	output, err := c.capture(ctx, path, args...)
	if err != nil {
		return nil, err
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) != 2 || ValidateSHA(parts[1]) != nil {
			return nil, fmt.Errorf("unexpected ref output %q", line)
		}
		refs[parts[0]] = parts[1]
	}
	return refs, nil
}

// DiffRefs reports deterministic branch and tag changes, including deletion.
func DiffRefs(before, after map[string]string) []RefUpdate {
	names := make(map[string]struct{}, len(before)+len(after))
	for name := range before {
		names[name] = struct{}{}
	}
	for name := range after {
		names[name] = struct{}{}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	updates := make([]RefUpdate, 0, len(ordered))
	for _, name := range ordered {
		if before[name] != after[name] {
			updates = append(updates, RefUpdate{Ref: name, OldSHA: before[name], NewSHA: after[name]})
		}
	}
	return updates
}

// Serve runs one allowlisted Git service directly, with no shell involved.
func (c *Cache) Serve(ctx context.Context, input string, service Service, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	parsed, err := ParseService(string(service))
	if err != nil {
		return err
	}
	if parsed == ReceivePack {
		return errors.New("receive-pack requires the durable Receive transaction")
	}
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if !c.isBare(ctx, path) {
		return fmt.Errorf("repository %s is not cached", repo)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		return fmt.Errorf("clean replacement refs for %s: %w", repo, err)
	}
	return c.serveLocked(ctx, path, parsed, protocol, stdin, stdout, stderr)
}

func (c *Cache) serveLocked(ctx context.Context, path string, service Service, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	extra := map[string]string{}
	if protocol != "" {
		extra["GIT_PROTOCOL"] = protocol
	}
	return c.run(ctx, commandSpec{
		dir: path, args: c.serviceArgs(service, path),
		stdin: stdin, stdout: stdout, stderr: stderr, env: extra,
	})
}

func (c *Cache) serviceArgs(service Service, path string) []string {
	args := []string{
		"-c", "core.useReplaceRefs=false",
		"-c", "uploadpack.hideRefs=refs/oberth/",
		"-c", "uploadpack.hideRefs=refs/replace/",
	}
	if service == ReceivePack {
		// HEAD is the one pseudo-ref receive-pack may otherwise recognize. Hide
		// it along with every refs/* namespace, then explicitly unhide the two
		// FAB public namespaces. Git rejects attempts to update a hidden ref.
		args = append(args,
			"-c", "receive.hideRefs=HEAD",
			"-c", "receive.hideRefs=refs/",
			"-c", "receive.hideRefs=!refs/heads/",
			"-c", "receive.hideRefs=!refs/tags/",
		)
	}
	return append(args, strings.TrimPrefix(string(service), "git-"), path)
}

func cloneMap(source map[string]string) map[string]string {
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
