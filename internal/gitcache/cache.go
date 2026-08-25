package gitcache

import (
	"bytes"
	"context"
	"encoding/json"
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

const materializeManifestVersion = 1
const materializeManifestFile = "oberth-materialize.json"

// materializeManifest is the durable ownership record for public-ref
// materialization. It lives inside each bare repository directory and
// survives process crashes. Without it, a partial materialize failure
// followed by a fetch that advances tracking refs causes the next Ensure
// to misclassify stale public refs as client-owned divergence, leaving
// them permanently stale.
type materializeManifest struct {
	Version  int               `json:"version"`
	Tracking map[string]string `json:"tracking"` // complete upstream tracking snapshot at last materialize
	Owned    map[string]string `json:"owned"`    // public ref -> SHA we set it to
}

var (
	receivePreReceiveHook = strings.NewReplacer(
		"__MAX_RECEIVE_REF_UPDATES__", fmt.Sprint(maximumReceiveRefUpdates),
		"__MAX_PUBLIC_REFS__", fmt.Sprint(maximumPublicRefs),
	).Replace(receivePreReceiveHookTemplate)
	receiveUpdateHook = strings.Replace(receiveUpdateHookTemplate, "__MAX_REF_PART_BYTES__", fmt.Sprint(maximumRefPartBytes), 1)
)

const receivePreReceiveHookTemplate = `#!/bin/sh
set -eu
LC_ALL=C
export LC_ALL

max_updates=__MAX_RECEIVE_REF_UPDATES__
max_public_refs=__MAX_PUBLIC_REFS__
# A receive cannot delete more refs than its command cap. Scanning one beyond
# public-cap + command-cap therefore stays bounded while proving that even the
# largest valid deletion batch cannot bring an oversized repository into range.
public_ref_scan_limit=$((max_public_refs + max_updates + 1))

is_zero_oid() {
  case "$1" in
    ''|*[!0]*) return 1 ;;
    *) return 0 ;;
  esac
}

reject_receive() {
  printf 'oberth: rejected receive: %s\n' "$1" >&2
  exit 1
}

hook_dir=$(git rev-parse --git-path hooks) || reject_receive "cannot resolve server hook directory"
public_refs_file=$(mktemp "$hook_dir/.oberth-public-refs.XXXXXX") || reject_receive "cannot allocate public-ref snapshot"
seen_refs_file=$(mktemp "$hook_dir/.oberth-seen-refs.XXXXXX") || {
  rm -f "$public_refs_file"
  reject_receive "cannot allocate receive-command snapshot"
}
trap 'rm -f "$public_refs_file" "$seen_refs_file"' 0
trap 'exit 1' 1 2 15

git for-each-ref --count="$public_ref_scan_limit" --format='%(objectname) %(refname)' refs/heads/ refs/tags/ >"$public_refs_file" ||
  reject_receive "cannot inspect current public refs"
public_count=$(wc -l <"$public_refs_file") || reject_receive "cannot count current public refs"
public_count=$((public_count + 0))
public_delta=0
update_count=0
over_limit=0
[ "$public_count" -le "$max_public_refs" ] || over_limit=1

while IFS=' ' read -r old new ref; do
  [ -n "$old" ] && [ -n "$new" ] && [ -n "$ref" ] || reject_receive "malformed ref update"
  update_count=$((update_count + 1))
  [ "$update_count" -le "$max_updates" ] || reject_receive "receive exceeds $max_updates ref updates"

  if grep -Fqx "$ref" "$seen_refs_file"; then
    reject_receive "duplicate update for $ref"
  else
    grep_status=$?
    [ "$grep_status" -eq 1 ] || reject_receive "cannot validate receive command uniqueness"
  fi
  printf '%s\n' "$ref" >>"$seen_refs_file" || reject_receive "cannot record receive command"

  case "$ref" in
    refs/heads/*|refs/tags/*)
      current=
      if current=$(awk -v target="$ref" '$2 == target { print $1; found=1; exit } END { if (!found) exit 1 }' "$public_refs_file"); then
        :
      else
        lookup_status=$?
        [ "$lookup_status" -eq 1 ] || reject_receive "cannot inspect current value of $ref"
        current=
      fi

      if [ "$over_limit" -eq 1 ]; then
        case "$ref" in
          refs/heads/*)
            if ! is_zero_oid "$new" || [ -z "$current" ] || [ "$old" != "$current" ]; then
              reject_receive "repository exceeds $max_public_refs public refs; recovery receive may contain only exact-current branch deletions"
            fi
            public_delta=$((public_delta - 1))
            ;;
          *)
            reject_receive "repository exceeds $max_public_refs public refs; recovery receive may contain only exact-current branch deletions"
            ;;
        esac
      elif is_zero_oid "$new"; then
        case "$ref" in
          refs/heads/*)
            if [ -n "$current" ] && [ "$old" = "$current" ]; then
              public_delta=$((public_delta - 1))
            fi
            ;;
        esac
      elif is_zero_oid "$old" && [ -z "$current" ]; then
        public_delta=$((public_delta + 1))
      fi
      ;;
    *)
      [ "$over_limit" -eq 0 ] ||
        reject_receive "repository exceeds $max_public_refs public refs; recovery receive may contain only exact-current branch deletions"
      ;;
  esac
done

resulting_public_count=$((public_count + public_delta))
[ "$resulting_public_count" -le "$max_public_refs" ] ||
  reject_receive "receive would leave $resulting_public_count public refs; limit is $max_public_refs"
`

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
	root            string
	outboxRoot      string
	globalConfig    string
	upstream        func(string) (string, error)
	gitBinary       string
	timeout         time.Duration
	env             map[string]string
	redact          []string
	logger          Logger
	preFinalizeGate func(context.Context) error
	locks           sync.Map
	receiveLocks    sync.Map
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
	globalConfig := filepath.Join(root, managedGitConfigFile)
	if err := writeManagedGitConfig(globalConfig); err != nil {
		return nil, fmt.Errorf("write managed Git configuration: %w", err)
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
		root:            root,
		outboxRoot:      outboxRoot,
		globalConfig:    globalConfig,
		upstream:        config.Upstream,
		gitBinary:       gitBinary,
		timeout:         defaultTimeout(config.CommandTimeout),
		env:             cloneMap(config.Env),
		redact:          append([]string(nil), config.Redact...),
		logger:          logger,
		preFinalizeGate: config.PreFinalizeGate,
	}, nil
}

// Ensure creates or refreshes one cache. A refresh failure is non-fatal only
// after a valid bare repository already exists. The input may be org-qualified
// ("org/repo") or a bare name; the org prefix routes upstream selection.
func (c *Cache) Ensure(ctx context.Context, input string) (Repository, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return Repository{}, err
	}
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	return c.ensureLocked(ctx, input, repo, path)
}

func (c *Cache) ensureLocked(ctx context.Context, input, repo, path string) (Repository, error) {
	return c.ensureLockedMayRecover(ctx, input, repo, path, false)
}

func (c *Cache) ensureReceiveLocked(ctx context.Context, input, repo, path string) (Repository, error) {
	return c.ensureLockedMayRecover(ctx, input, repo, path, true)
}

func (c *Cache) ensureLockedMayRecover(ctx context.Context, input, repo, path string, detectRecovery bool) (Repository, error) {
	if stat, err := os.Stat(path); err == nil {
		if !stat.IsDir() || !c.isBare(ctx, path) {
			return Repository{}, fmt.Errorf("cache path for %s is not a bare repository", repo)
		}
		if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
			return Repository{}, fmt.Errorf("clean replacement refs for %s: %w", repo, err)
		}
		publicRefCount, err := c.cleanInvalidPublicRefsLocked(ctx, path)
		if err != nil {
			return Repository{}, fmt.Errorf("clean invalid public refs for %s: %w", repo, err)
		}
		if err := installReceiveHooks(path); err != nil {
			return Repository{}, fmt.Errorf("install receive policy for %s: %w", repo, err)
		}
		if detectRecovery && publicRefCount > maximumPublicRefs {
			expectedRemote, err := c.validatedUpstream(input)
			if err != nil {
				return Repository{}, err
			}
			cachedRemote, remoteErr := c.capture(ctx, path, "remote", "get-url", "upstream")
			if remoteErr != nil {
				return Repository{}, fmt.Errorf("read cached upstream for public-ref recovery: %w", remoteErr)
			}
			if cachedRemote = strings.TrimSpace(cachedRemote); cachedRemote != expectedRemote {
				return Repository{}, fmt.Errorf(
					"upstream identity for %s changed from %s to %s; refusing public-ref recovery across trust boundaries",
					repo, cachedRemote, expectedRemote,
				)
			}
			branch, branchErr := c.currentDefaultBranch(ctx, path)
			if branchErr != nil {
				return Repository{}, fmt.Errorf("read cached default branch for public-ref recovery: %w", branchErr)
			}
			c.logger.Printf("repository %s has %d public refs; accepting deletion-only recovery receive", repo, publicRefCount)
			return Repository{Path: path, DefaultBranch: branch, recoveryOnly: true}, nil
		}
		// Record the remote URL before refresh may change it, so the stale
		// fallback can verify it is returning data from the same upstream.
		preRefreshRemote, _ := c.capture(ctx, path, "remote", "get-url", "upstream")
		preRefreshRemote = strings.TrimSpace(preRefreshRemote)

		branch, refreshErr := c.refresh(ctx, input, path)
		if refreshErr != nil {
			// Check whether the upstream identity changed during the
			// failed refresh. If it did, the stale cache belongs to a
			// different upstream and must not be served — doing so would
			// let a re-registered repo name see refs from the old upstream.
			postRefreshRemote, _ := c.capture(ctx, path, "remote", "get-url", "upstream")
			postRefreshRemote = strings.TrimSpace(postRefreshRemote)
			if preRefreshRemote != "" && postRefreshRemote != "" && preRefreshRemote != postRefreshRemote {
				return Repository{}, fmt.Errorf(
					"upstream identity for %s changed from %s to %s during a failed refresh; "+
						"refusing stale fallback across trust boundaries: %w",
					repo, preRefreshRemote, postRefreshRemote, refreshErr)
			}

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
	if err := c.configureRemote(ctx, input, temporary); err != nil {
		return Repository{}, err
	}
	if err := c.fetchTracking(ctx, temporary); err != nil {
		// Log the fetch failure so it is visible in the server's structured log
		// even though no receive event, audit action, or CI issue is created at
		// this point — the fetch fails before any reservation exists (issue #212
		// part 4). The error now includes the forge's stderr (part 1), so the
		// operator sees the actual reason rather than a bare exit code.
		c.logger.Printf("initial upstream fetch failed for %s: %v", repo, err)
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
	if err := installReceiveHooks(temporary); err != nil {
		return Repository{}, fmt.Errorf("install receive policy for %s: %w", repo, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return Repository{}, fmt.Errorf("publish cache for %s: %w", repo, err)
	}
	return Repository{Path: path, DefaultBranch: branch}, nil
}

func installReceiveHooks(repository string) error {
	hooks := filepath.Join(repository, "hooks")
	info, err := os.Lstat(hooks)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("git hooks path is not a directory")
	}
	if err := installHookFile(hooks, "pre-receive", receivePreReceiveHook); err != nil {
		return err
	}
	return installHookFile(hooks, "update", receiveUpdateHook)
}

func installHookFile(hooksDir, name, content string) error {
	target := filepath.Join(hooksDir, name)
	// #nosec G304 -- target is hooks/<name> under the validated cache root.
	existing, err := os.ReadFile(target)
	if err == nil && string(existing) == content {
		return nil // Skip unchanged hook writes.
	}
	temporary, err := os.CreateTemp(hooksDir, ".oberth-"+name+"-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if _, err := io.WriteString(temporary, content); err != nil {
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
	if err := os.Rename(temporaryPath, filepath.Join(hooksDir, name)); err != nil {
		return err
	}
	return syncDirectory(hooksDir)
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

func (c *Cache) refresh(ctx context.Context, input, path string) (string, error) {
	old, err := c.listRefs(ctx, path, upstreamRefPrefix)
	if err != nil {
		return "", err
	}
	if err := c.configureRemote(ctx, input, path); err != nil {
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

func (c *Cache) configureRemote(ctx context.Context, input, path string) error {
	remote, err := c.validatedUpstream(input)
	if err != nil {
		return err
	}
	if _, err := c.capture(ctx, path, "remote", "get-url", "upstream"); err != nil {
		return c.run(ctx, commandSpec{dir: path, args: []string{"remote", "add", "upstream", remote}})
	}
	return c.run(ctx, commandSpec{dir: path, args: []string{"remote", "set-url", "upstream", remote}})
}

func (c *Cache) validatedUpstream(input string) (string, error) {
	remote, err := c.upstream(input)
	if err != nil {
		return "", fmt.Errorf("resolve upstream for %s: %w", input, err)
	}
	if strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf("upstream for %s is empty", input)
	}
	if err := ValidateUpstream(remote); err != nil {
		return "", fmt.Errorf("upstream for %s: %w", input, err)
	}
	return remote, nil
}

func (c *Cache) fetchTracking(ctx context.Context, path string) error {
	return c.run(ctx, commandSpec{dir: path, args: []string{
		"fetch", "--prune", "--no-tags", "upstream",
		"+refs/heads/*:" + upstreamRefPrefix + "heads/*",
		"+refs/tags/*:" + upstreamRefPrefix + "tags/*",
	}})
}

// materialize synchronizes public refs from upstream tracking refs.
//
// Reader-consistency note: each individual ref update (git update-ref) is
// atomic on the files backend — a concurrent reader observes either the old
// SHA or the new SHA, never a torn value. Objects always precede refs because
// fetch (which writes objects) always precedes materialize (which writes refs),
// so a ref never points at a missing object. However, the files backend
// provides NO cross-ref snapshot isolation: a reader during materialization may
// observe ref A at its new value and ref B at its old value. Full multi-ref
// snapshot atomicity requires a generational or indirection-based redesign and
// is explicitly out of scope; eventual convergence is guaranteed because every
// Ensure re-runs materialize until tracking and owned maps agree.
func (c *Cache) materialize(ctx context.Context, path string, old map[string]string) error {
	manifest := c.readMaterializeManifest(path)

	current, err := c.listRefs(ctx, path, upstreamRefPrefix)
	if err != nil {
		return err
	}

	// Unchanged-upstream check: if the tracking-ref snapshot is identical to
	// the manifest, no materialization work is needed and the owned map is
	// already correct.
	if manifest.Tracking != nil && trackingMapsEqual(manifest.Tracking, current) {
		return nil
	}

	public, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/", "refs/tags/")
	if err != nil {
		return err
	}

	newOwned := make(map[string]string, len(current))

	// Phase 1: Delete public refs whose tracking ref was removed upstream.
	// Deletes run before creates to respect the public-ref ceiling.
	deletions := make([]string, 0, len(manifest.Owned))
	for ref := range manifest.Owned {
		tracking := reversePublicRef(ref)
		if tracking == "" || current[tracking] != "" {
			continue
		}
		deletions = append(deletions, ref)
	}
	sort.Strings(deletions)
	deletedCount := 0
	for _, ref := range deletions {
		ownedSHA := manifest.Owned[ref]
		publicSHA, publicExists := public[ref]
		if !publicExists || publicSHA != ownedSHA {
			continue // Already gone or client modified.
		}
		if err := c.deleteRef(ctx, path, ref, ownedSHA); err != nil {
			return err
		}
		deletedCount++
	}

	// Phase 2: Create or update public refs for current tracking refs.
	// Creates are collected separately so the public-ref ceiling can truncate
	// them without affecting owned-ref updates.
	type pendingCreate struct {
		ref    string
		newSHA string
	}
	var creates []pendingCreate

	trackingRefs := make([]string, 0, len(current))
	for tracking := range current {
		trackingRefs = append(trackingRefs, tracking)
	}
	sort.Strings(trackingRefs)

	for _, tracking := range trackingRefs {
		newSHA := current[tracking]
		ref, ok := publicRef(tracking)
		if !ok {
			continue
		}
		publicSHA, publicExists := public[ref]

		if !publicExists {
			creates = append(creates, pendingCreate{ref: ref, newSHA: newSHA})
			continue
		}

		// Determine ownership using the durable manifest, then crash-
		// recovery heuristics, then the pre-fetch snapshot fallback.
		owned := false
		switch {
		case manifest.Owned[ref] != "" && publicSHA == manifest.Owned[ref]:
			// Public ref matches what we last materialized.
			owned = true
		case publicSHA == newSHA:
			// Already at the target value. Covers crash recovery (ref
			// updated but manifest not written) and the no-op case.
			owned = true
		case old != nil:
			// Pre-fetch tracking snapshot: catches the crash window where
			// a ref was updated but the manifest was not written, and
			// upstream then advanced again.
			oldTracking, existed := old[tracking]
			if existed && publicSHA == oldTracking {
				owned = true
			}
		}

		if !owned {
			continue
		}

		if publicSHA != newSHA {
			if err := c.updateRef(ctx, path, ref, newSHA, publicSHA); err != nil {
				return err
			}
		}
		newOwned[ref] = newSHA
	}

	// Enforce the public-ref ceiling on creates. Phase 1 deletes already ran,
	// so the remaining public count is the original minus actual deletions.
	// Creates are in sorted tracking-ref order, which maps deterministically
	// to sorted public-ref order (both share the suffix after their prefix).
	// Truncation policy: alphabetically first refs win; refs beyond the cap
	// are dropped from this cycle. A future Ensure with fewer public refs
	// picks them up.
	currentPublicCount := len(public) - deletedCount
	available := maximumPublicRefs - currentPublicCount
	if available < 0 {
		available = 0
	}
	if len(creates) > available {
		c.logger.Printf("upstream materialization truncated: %d new refs exceed cap (%d current, %d limit); %d refs dropped",
			len(creates), currentPublicCount, maximumPublicRefs, len(creates)-available)
		creates = creates[:available]
	}

	for _, create := range creates {
		if err := c.updateRef(ctx, path, create.ref, create.newSHA, ""); err != nil {
			return err
		}
		newOwned[create.ref] = create.newSHA
	}

	return c.writeMaterializeManifest(path, current, newOwned)
}

// cleanInvalidPublicRefsLocked removes public refs with names that fail
// validation and returns the count of valid public refs remaining. The scan
// is bounded by maximumReceiveSnapshotRefs to stay within the capture buffer
// while accommodating both the normal cap and one recovery deletion batch.
func (c *Cache) cleanInvalidPublicRefsLocked(ctx context.Context, path string) (int, error) {
	public, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/", "refs/tags/")
	if err != nil {
		return 0, err
	}
	var invalidRefs []string
	for ref := range public {
		if !publicReceiveRef(ref) {
			invalidRefs = append(invalidRefs, ref)
		}
	}
	sort.Strings(invalidRefs)
	for _, ref := range invalidRefs {
		if err := c.deleteRef(ctx, path, ref, public[ref]); err != nil {
			return 0, fmt.Errorf("delete invalid public ref %s: %w", ref, err)
		}
	}
	return len(public) - len(invalidRefs), nil
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

// reversePublicRef maps a public ref back to the tracking ref it was
// materialized from. Returns "" if the ref is not a valid public branch or tag.
func reversePublicRef(ref string) string {
	switch {
	case strings.HasPrefix(ref, "refs/heads/"):
		branch := strings.TrimPrefix(ref, "refs/heads/")
		if ValidateBranch(branch) != nil {
			return ""
		}
		return upstreamRefPrefix + "heads/" + branch
	case strings.HasPrefix(ref, "refs/tags/"):
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if ValidateTag(tag) != nil {
			return ""
		}
		return upstreamRefPrefix + "tags/" + tag
	default:
		return ""
	}
}

func trackingMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, valueA := range a {
		if b[key] != valueA {
			return false
		}
	}
	return true
}

func (c *Cache) readMaterializeManifest(path string) materializeManifest {
	file := filepath.Join(path, materializeManifestFile)
	// path is the validated bare-repository directory under the cache root.
	body, err := os.ReadFile(file) //nolint:gosec
	if err != nil {
		return materializeManifest{Version: materializeManifestVersion}
	}
	var m materializeManifest
	if json.Unmarshal(body, &m) != nil || m.Version != materializeManifestVersion {
		return materializeManifest{Version: materializeManifestVersion}
	}
	return m
}

func (c *Cache) writeMaterializeManifest(path string, tracking, owned map[string]string) error {
	m := materializeManifest{
		Version:  materializeManifestVersion,
		Tracking: tracking,
		Owned:    owned,
	}
	body, err := json.Marshal(m)
	if err != nil {
		return fmt.Errorf("encode materialization manifest: %w", err)
	}
	file := filepath.Join(path, materializeManifestFile)
	temporary, err := os.CreateTemp(path, ".oberth-materialize-*.tmp")
	if err != nil {
		return fmt.Errorf("create materialization manifest: %w", err)
	}
	temporaryPath := temporary.Name()
	clean := false
	defer func() {
		_ = temporary.Close()
		if !clean {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temporary.Write(body); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, file); err != nil {
		return err
	}
	clean = true
	return syncDirectory(path)
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
	_, repo, err := ParseRepoPath(input)
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

// RemoveRepository deletes the bare cache directory for one repository,
// serializing with any in-flight receive or refresh on the same repository
// through the same locks those paths hold (receiveLock outer, repoLock
// inner — the receive lifecycle's own ordering). The input is re-validated
// and the resulting path is confined to the cache root by c.path before
// anything is removed; a name that fails validation removes nothing. A
// missing directory is success: removal is idempotent, and the next push's
// Ensure recreates the cache from the upstream.
func (c *Cache) RemoveRepository(input string) error {
	repo, path, err := c.path(input)
	if err != nil {
		return err
	}
	receiveLock := c.receiveLock(repo)
	receiveLock.Lock()
	defer receiveLock.Unlock()
	lock := c.repoLock(repo)
	lock.Lock()
	defer lock.Unlock()
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove repository cache: %w", err)
	}
	return nil
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
	return c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/", "refs/tags/")
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
	var buf bytes.Buffer
	if err := c.run(ctx, commandSpec{dir: path, args: args, stdout: &buf, stderr: &buf}); err != nil {
		return nil, err
	}
	if buf.Len() > maxRefsOutput {
		return nil, fmt.Errorf("ref output exceeds %d bytes (%d bytes); too many refs", maxRefsOutput, buf.Len())
	}
	refs := make(map[string]string)
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
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
		// public namespaces. Git rejects attempts to update a hidden ref.
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
