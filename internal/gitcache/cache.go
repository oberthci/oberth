package gitcache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/boundedio"
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

// Cache owns bare repositories under one persistent root.
type Cache struct {
	// Hook read-only children stay in the enclosing receive process group.
	allowLocalUpstreams   bool
	hookChild             bool
	backend               gitBackend
	root                  string
	outboxRoot            string
	globalConfig          string
	upstream              func(string) (string, error)
	qualify               func(string) (RepoQualification, error)
	qualifyContext        func(context.Context, string) (RepoQualification, error)
	gitBinary             string
	timeout               time.Duration
	env                   map[string]string
	redact                []string
	logger                Logger
	repositoryGate        func(context.Context, string) error
	promotionPinAbandoned func(context.Context, string, string, string) (bool, error)
	preFinalizeGate       func(context.Context) error
	reservedBranches      []string
	locks                 sync.Map
	receiveLocks          sync.Map
	policyRefreshes       sync.Map         // cache path -> policyRefresh; guarded by repoLock
	policyClock           func() time.Time // Optional freshness clock; set before concurrent use.
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
	reserved, err := ReservedBranchPatterns(config.ReservedBranches)
	if err != nil {
		return nil, err
	}
	cache := &Cache{
		reservedBranches:      reserved,
		allowLocalUpstreams:   config.AllowLocalUpstreams,
		root:                  root,
		outboxRoot:            outboxRoot,
		globalConfig:          globalConfig,
		upstream:              config.Upstream,
		qualify:               config.RepoQualifier,
		qualifyContext:        config.RepoQualifierContext,
		gitBinary:             gitBinary,
		timeout:               defaultTimeout(config.CommandTimeout),
		env:                   cloneMap(config.Env),
		redact:                append([]string(nil), config.Redact...),
		logger:                logger,
		preFinalizeGate:       config.PreFinalizeGate,
		repositoryGate:        config.RepositoryGate,
		promotionPinAbandoned: config.PromotionPinAbandoned,
	}
	cache.backend = &cliBackend{cache: cache}
	return cache, nil
}

// Ensure creates or refreshes one cache. A refresh failure is non-fatal only
// after a valid bare repository already exists. The input may be org-qualified
// ("org/repo") or a bare name; the org prefix routes upstream selection.
func (c *Cache) Ensure(ctx context.Context, input string) (Repository, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return Repository{}, err
	}
	lock := c.repoLock(path)
	if err := lock.LockContext(ctx); err != nil {
		return Repository{}, err
	}
	defer lock.Unlock()
	identity := c.reservationIdentity(repo, path)
	if reservation, err := c.readReservation(identity); err == nil && (reservation.State == receiveReserved || reservation.State == receiveGateFailed) {
		if err := c.checkRepositoryActive(ctx, input); err != nil {
			return Repository{}, err
		}
		return c.ensureCachedOnlyLocked(ctx, repo, path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Repository{}, err
	}
	return c.ensureLocked(ctx, input, repo, path)
}

// EnsureForFetch is the clone/fetch entry point. It respects the receive
// invariant (receive.go:62-64): when a pending receive reservation exists,
// upstream refs must not be refreshed because unrelated upstream movement
// could be misattributed to the prior actor's crash window. The cache is
// served stale until the reservation is recovered by ReplayPending or the
// next Receive (#435).
//
// Receive handlers that re-enter the cache to inspect accepted objects must
// call Ensure (not EnsureForFetch) because they run inside the reservation
// lifecycle and legitimately need a fresh refresh.
func (c *Cache) EnsureForFetch(ctx context.Context, input string) (Repository, error) {
	repo, path, err := c.path(input)
	if err != nil {
		return Repository{}, err
	}
	lock := c.repoLock(path)
	if err := ctx.Err(); err != nil {
		return Repository{}, err
	}
	if !lock.TryLock() {
		if err := lock.RLockContext(ctx); err != nil {
			return Repository{}, err
		}
		defer lock.RUnlock()
		if err := c.checkRepositoryActive(ctx, input); err != nil {
			return Repository{}, err
		}
		if !c.isBare(ctx, path) {
			return Repository{}, fmt.Errorf("repository %s is not cached", repo)
		}
		expected, err := c.validatedUpstream(input)
		if err != nil {
			return Repository{}, err
		}
		actual, err := c.capture(ctx, path, "remote", "get-url", "upstream")
		if err != nil {
			return Repository{}, err
		}
		if expected != actual {
			return Repository{}, errors.New("cached upstream identity changed while repository is busy")
		}
		branch, err := c.currentDefaultBranch(ctx, path)
		return Repository{Path: path, DefaultBranch: branch, Stale: true}, err
	}
	defer lock.Unlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return Repository{}, err
	}
	// A pending reservation means a crashed or gate-failed receive has not
	// been recovered yet. Serve the cache without refreshing.
	identity := c.reservationIdentity(repo, path)
	if _, readErr := c.readReservation(identity); readErr == nil {
		return c.ensureCachedOnlyLocked(ctx, repo, path)
	}
	return c.ensureLocked(ctx, input, repo, path)
}

// ensureCachedOnlyLocked returns the cached repository without refreshing
// upstream. It requires repoLock to be held. This path is used when a
// pending receive reservation exists and the caller cannot replay it
// (e.g. a clone/fetch with no receive handler).
func (c *Cache) ensureCachedOnlyLocked(ctx context.Context, repo, path string) (Repository, error) {
	if err := sweepReceiveQuarantines(path, time.Now()); err != nil {
		c.logger.Printf("git receive quarantine cleanup failed: repository=%q error=%v", path, err)
	}

	if stat, err := os.Stat(path); err != nil || !stat.IsDir() || !c.isBare(ctx, path) {
		return Repository{}, fmt.Errorf("repository %s is not cached and cannot refresh while a receive reservation is pending", repo)
	}
	if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
		return Repository{}, fmt.Errorf("clean replacement refs for %s: %w", repo, err)
	}
	if _, err := c.cleanInvalidPublicRefsLocked(ctx, path); err != nil {
		return Repository{}, fmt.Errorf("clean invalid public refs for %s: %w", repo, err)
	}
	branch, err := c.currentDefaultBranch(ctx, path)
	if err != nil {
		return Repository{}, fmt.Errorf("read default branch for %s: %w", repo, err)
	}
	return Repository{Path: path, DefaultBranch: branch, Stale: true}, nil
}

func (c *Cache) ensureLocked(ctx context.Context, input, repo, path string) (Repository, error) {
	return c.ensureLockedMayRecover(ctx, input, repo, path, false)
}

func (c *Cache) ensureReceiveLocked(ctx context.Context, input, repo, path string) (Repository, error) {
	return c.ensureLockedMayRecover(ctx, input, repo, path, true)
}

func (c *Cache) ensureLockedMayRecover(ctx context.Context, input, repo, path string, detectRecovery bool) (Repository, error) {
	started := c.policyNow()
	// A failed refresh must not leave an earlier successful policy snapshot
	// usable by this admission attempt, even when Git can serve stale refs.
	c.policyRefreshes.Delete(path)
	if err := sweepReceiveQuarantines(path, time.Now()); err != nil {
		c.logger.Printf("git receive quarantine cleanup failed: repository=%q error=%v", path, err)
	}

	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return Repository{}, err
	}
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
			// A refused upstream resolution (org or upstream-name mismatch)
			// must fail closed immediately. The cache belongs to the correct
			// upstream; the request is for a different identity and must
			// never see its contents — serving stale data here would let a
			// mismatched push path read refs it has no claim to.
			if errors.Is(refreshErr, ErrUpstreamRefused) {
				return Repository{}, refreshErr
			}
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

			c.logger.Printf("upstream refresh unavailable for %s; serving stale cache: %v", repo, refreshErr)
			// Cached policy readers (push admission) then report this cause
			// for the failure cooldown instead of a missing snapshot.
			if ctx.Err() == nil {
				c.rememberFailedRefreshLocked(path, postRefreshRemote, started, refreshErr)
			}
			branch, err = c.currentDefaultBranch(ctx, path)
			if err != nil {
				return Repository{}, errors.Join(refreshErr, fmt.Errorf("read cached default branch: %w", err))
			}
			return Repository{Path: path, Stale: true, StaleReason: staleReason(refreshErr), DefaultBranch: branch}, nil
		}
		c.rememberPushPolicyLocked(ctx, path, branch, started)
		return Repository{Path: path, DefaultBranch: branch}, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return Repository{}, fmt.Errorf("inspect cache for %s: %w", repo, err)
	}

	temporary, err := os.MkdirTemp(c.root, "."+repo+"-init-")
	if err != nil {
		return Repository{}, fmt.Errorf("create temporary cache: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()
	// HEAD starts on main: an unborn upstream keeps the cache's default.
	initArguments := []string{"init", "--bare", "--initial-branch=main", temporary}
	if err := c.run(ctx, commandSpec{repository: input, args: initArguments}); err != nil {
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
		return Repository{}, fmt.Errorf("initial upstream fetch for %s: %w%s", repo, err, c.missingRepositoryHint(ctx, temporary, err))
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
	// The qualified layout nests caches under <upstream>/<org>/; the parent
	// directories must exist before the atomic rename publishes the cache.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return Repository{}, fmt.Errorf("create cache parent for %s: %w", repo, err)
	}
	if err := os.Rename(temporary, path); err != nil {
		return Repository{}, fmt.Errorf("publish cache for %s: %w", repo, err)
	}
	c.rememberPushPolicyLocked(ctx, path, branch, started)
	return Repository{Path: path, DefaultBranch: branch}, nil
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
	// A brand-new empty upstream has an unborn default branch, so no
	// tracking ref exists yet (issue #264 bootstrap). That is not an error:
	// it means there is NO admission anchor — the empty admission below
	// keeps every release tag refused (a tag must be reachable from the
	// fresh upstream default branch, and nothing is reachable from an
	// unborn branch) while branch receives proceed. The check is
	// existence-based, never a guess: any OTHER rev-parse failure still
	// refuses the receive.
	if _, refErr := c.capture(ctx, path, "show-ref", "--verify", "--quiet", upstreamRef); refErr != nil {
		if unborn, listErr := c.defaultBranchUnborn(ctx, path, defaultBranch); listErr == nil && unborn {
			return ReleaseAdmission{}, c.deleteRef(ctx, path, admissionRef, "")
		}
	}
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
	if err := c.migratePromotionPins(ctx, path); err != nil {
		return "", err
	}
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
	if err := c.sweepDeliveredPromotionPins(ctx, input, path); err != nil {
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
		err = c.run(ctx, commandSpec{dir: path, args: []string{"remote", "add", "upstream", remote}})
		if err != nil {
			return err
		}
	} else if err := c.run(ctx, commandSpec{dir: path, args: []string{"remote", "set-url", "upstream", remote}}); err != nil {
		return err
	}
	// git remote add configures +refs/heads/*:refs/remotes/upstream/*. With
	// it, every fetch and push opportunistically writes refs/remotes/upstream/*
	// in the mirror, which no prune removes and upload-pack advertises, so a
	// deleted branch stays served (#1504). Oberth fetches only explicit
	// refspecs into refs/oberth/upstream/, so the remote needs none.
	if _, err := c.capture(ctx, path, "config", "--get-all", "remote.upstream.fetch"); err == nil {
		return c.run(ctx, commandSpec{dir: path, args: []string{"config", "--unset-all", "remote.upstream.fetch"}})
	}
	return nil
}

func (c *Cache) validatedUpstream(input string) (string, error) {
	remote, err := c.upstream(input)
	if err != nil {
		return "", fmt.Errorf("resolve upstream for %s: %w", input, err)
	}
	if strings.TrimSpace(remote) == "" {
		return "", fmt.Errorf("upstream for %s is empty", input)
	}
	if filepath.IsAbs(remote) && !c.allowLocalUpstreams {
		return "", fmt.Errorf("%w: local filesystem upstreams are disabled", ErrUpstreamRefused)
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

	deleting, err := c.deletingBranches(ctx, path)
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
			if _, deleting := deleting[ref]; deleting {
				// Accepted deletion still propagating upstream.
				continue
			}
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
// structural storage validation and returns the remaining public-ref count.
// Run-fact alphabet restrictions never delete stored refs. The scan
// is bounded by maximumReceiveSnapshotRefs to stay within the capture buffer
// while accommodating both the normal cap and one recovery deletion batch.
func (c *Cache) cleanInvalidPublicRefsLocked(ctx context.Context, path string) (int, error) {
	public, err := c.listRefsAtMost(ctx, path, maximumReceiveSnapshotRefs, "refs/heads/", "refs/tags/")
	if err != nil {
		return 0, err
	}
	var invalidRefs []string
	for ref := range public {
		if reversePublicRef(ref) == "" {
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
		if validateStoredRefPart("branch", branch) != nil {
			return "", false
		}
		return "refs/heads/" + branch, true
	case strings.HasPrefix(tracking, upstreamRefPrefix+"tags/"):
		tag := strings.TrimPrefix(tracking, upstreamRefPrefix+"tags/")
		if validateStoredRefPart("tag", tag) != nil {
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
		if validateStoredRefPart("branch", branch) != nil {
			return ""
		}
		return upstreamRefPrefix + "heads/" + branch
	case strings.HasPrefix(ref, "refs/tags/"):
		tag := strings.TrimPrefix(ref, "refs/tags/")
		if validateStoredRefPart("tag", tag) != nil {
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
	symrefOutput, err := c.capture(ctx, path, "ls-remote", "--symref", "upstream", "HEAD")
	if err != nil {
		return "", err
	}
	branch, err := defaultBranchFromSymref(symrefOutput)
	if err == nil {
		return branch, nil
	}
	if !errors.Is(err, errNoSymbolicDefaultBranch) {
		return "", err
	}
	// A HEAD that resolves to a commit without naming its branch (a server
	// without the symref capability) would make Oberth guess among existing
	// branches for the promotion target and the release anchor: refuse.
	if headAdvertised(symrefOutput) {
		return "", fmt.Errorf("%w: the upstream HEAD resolves to a commit but the forge does not say which branch it is; set the repository's default branch on the forge", ErrUpstreamDefaultBranch)
	}
	// No HEAD at all: the upstream's default branch is unborn. That is a
	// brand-new repository, empty or holding only branches Oberth already
	// synced (stock OpenSSH forges speak protocol v0, which never advertises
	// an unborn HEAD). Keep the default branch this cache already recorded
	// (the repository's registered default), or "main" for a new cache; the
	// first promotion or default-branch publication creates it upstream.
	branch, err = c.currentDefaultBranch(ctx, path)
	if err != nil {
		branch = "main"
	}
	return branch, nil
}

// ErrUpstreamDefaultBranch reports an upstream whose default branch Oberth
// cannot determine. It is a forge configuration problem, not a transient
// fault: retrying unchanged repeats it.
var ErrUpstreamDefaultBranch = errors.New("upstream default branch unknown")

// headAdvertised reports whether an `ls-remote --symref <remote> HEAD`
// answer carries a resolved HEAD row.
func headAdvertised(output string) bool {
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == "HEAD" {
			return true
		}
	}
	return false
}

// trackingBranchAbsent reports whether the last successful upstream fetch
// (which prunes) proved that branch does not exist upstream. Only a clean
// listing counts; an unreadable repository is never read as absent.
func (c *Cache) trackingBranchAbsent(ctx context.Context, path, branch string) (bool, error) {
	ref := upstreamRefPrefix + "heads/" + branch
	output, err := c.capture(ctx, path, "for-each-ref", "--format=%(refname)", ref)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(output, "\n") {
		if strings.TrimSpace(line) == ref {
			return false, nil
		}
	}
	return true, nil
}

// defaultBornFile marks a cache whose upstream default branch has existed.
const defaultBornFile = "oberth-default-born"

// recordDefaultBranchBorn durably notes that the upstream default branch
// exists, so its later disappearance is never read as a new repository.
func (c *Cache) recordDefaultBranchBorn(ctx context.Context, path, branch string) error {
	marker := filepath.Join(path, defaultBornFile)
	if _, err := os.Stat(marker); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read default-branch marker: %w", err)
	}
	if absent, err := c.trackingBranchAbsent(ctx, path, branch); err != nil || absent {
		return err
	}
	if err := os.WriteFile(marker, []byte(branch+"\n"), 0o600); err != nil {
		return fmt.Errorf("write default-branch marker: %w", err)
	}
	return syncDirectory(path)
}

// defaultBranchUnborn reports whether branch is an unborn default: the last
// successful (pruning) fetch proved it absent upstream, and either this cache
// never saw a default branch upstream (a new repository, empty or holding
// only other branches) or the upstream now holds no refs at all. A default
// branch that existed and vanished beside other refs was deleted; treating
// it as unborn would silently drop its policy and let the next push or
// promotion recreate it ungoverned.
func (c *Cache) defaultBranchUnborn(ctx context.Context, path, branch string) (bool, error) {
	absent, err := c.trackingBranchAbsent(ctx, path, branch)
	if err != nil || !absent {
		return false, err
	}
	if _, err := os.Stat(filepath.Join(path, defaultBornFile)); errors.Is(err, os.ErrNotExist) {
		return true, nil
	} else if err != nil {
		return false, err
	}
	refs, err := c.capture(ctx, path, "for-each-ref", "--count=1", upstreamRefPrefix)
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(refs) == "", nil
}

// errNoSymbolicDefaultBranch reports an upstream advertisement that carries
// no symbolic HEAD; discoverDefaultBranch decides whether that is the unborn
// bootstrap case or a hard error.
var errNoSymbolicDefaultBranch = errors.New("upstream did not advertise a symbolic default branch")

// defaultBranchFromSymref extracts the default branch from
// `ls-remote --symref <remote> HEAD` output.
func defaultBranchFromSymref(output string) (string, error) {
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
	return "", errNoSymbolicDefaultBranch
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
	if err := c.recordDefaultBranchBorn(ctx, path, branch); err != nil {
		return err
	}
	if _, err := c.capture(ctx, path, "show-ref", "--verify", "--quiet", ref); err != nil {
		// Unborn-branch bootstrap: the upstream has no default branch yet,
		// so it cannot exist in the cache either. Point HEAD at the unborn
		// branch only when the pruning fetch proved it absent upstream — a
		// fetched branch missing from the public refs means materialization
		// failed — and this cache never saw a default branch there: one
		// that disappeared was deleted, and its policy must not lapse.
		absent, listErr := c.trackingBranchAbsent(ctx, path, branch)
		if listErr != nil || !absent {
			return fmt.Errorf("upstream default branch %s was not fetched: %w", branch, err)
		}
		unborn, listErr := c.defaultBranchUnborn(ctx, path, branch)
		if listErr != nil {
			return fmt.Errorf("upstream default branch %s was not fetched: %w", branch, listErr)
		}
		if !unborn {
			return fmt.Errorf("%w: default branch %s is missing upstream although this repository had a default branch before; restore it on the forge", ErrUpstreamDefaultBranch, branch)
		}
	}
	return c.run(ctx, commandSpec{dir: path, args: []string{"symbolic-ref", "HEAD", ref}})
}

func (c *Cache) isBare(ctx context.Context, path string) bool {
	output, err := c.capture(ctx, path, "rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(output) == "true"
}

func (c *Cache) path(input string) (string, string, error) {
	return c.pathWithQualifier(input, c.qualify)
}

func (c *Cache) pathWithQualifier(input string, qualify func(string) (RepoQualification, error)) (string, string, error) {
	upstream, org, repo, err := ParseRepoPath(input)
	if err != nil {
		return "", "", err
	}
	if qualify != nil {
		qualification, qualifyErr := qualify(input)
		if qualifyErr != nil {
			return "", "", fmt.Errorf("resolve cache identity for %q: %w", input, qualifyErr)
		}
		// The catalog's canonical spellings win over the client-cased
		// segments: org matching upstream is case-insensitive for operator
		// convenience, but the on-disk path must be byte-stable. A true
		// identity mismatch (wrong org for a registered repo) is refused by
		// the resolver itself before any disk path is derived.
		upstream, org = qualification.UpstreamName, qualification.Org
	}
	cachePath := c.qualifiedCachePath(upstream, org, repo)
	relative, err := filepath.Rel(c.root, cachePath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("repository path escapes cache root")
	}
	return repo, cachePath, nil
}

// qualifiedCachePath resolves the on-disk path for a repository's bare cache.
//
// With a full qualification the layout is <root>/<upstream>/<org>/<repo>.git,
// which lets same-named repositories under different upstreams coexist with
// strictly disjoint caches (issue #264): "codeberg/acme/terraform" and
// "github/oberthci/terraform" resolve to different directories, and every
// input form of ONE repository resolves to the same directory because the
// RepoQualifier returns catalog-canonical segments regardless of input form.
//
// Without a qualification (nil RepoQualifier in tests, or a legacy caller)
// the flat layout (<root>/<repo>.git) is preserved. Existing flat caches are
// moved to the qualified layout by MigrateToQualifiedLayout at server
// startup — see migrate_cache.go.
func (c *Cache) qualifiedCachePath(upstream, org, repo string) string {
	if upstream != "" && org != "" &&
		!strings.ContainsAny(upstream, "/\\") && !strings.ContainsAny(org, "/\\") &&
		upstream != "." && upstream != ".." && org != "." && org != ".." {
		return filepath.Join(c.root, upstream, org, repo+".git")
	}
	return filepath.Join(c.root, repo+".git")
}

// repoLock returns the mutex guarding one cache directory. Locks are keyed
// by the resolved cache path — never by the bare name — so every spelling of
// one repository shares a lock and same-named repositories under different
// upstreams do not contend or alias (issue #264).
func (c *Cache) repoLock(cachePath string) *repositoryLock {
	value, _ := c.locks.LoadOrStore(cachePath, newRepositoryLock())
	return value.(*repositoryLock)
}

// receiveLock serializes complete receive lifecycles, including durable
// callback delivery, without forcing callbacks to run under repoLock. Receive
// handlers need to inspect the accepted Git objects and therefore legitimately
// re-enter Cache methods that acquire repoLock themselves.
func (c *Cache) receiveLock(repo string) *repositoryLock {
	value, _ := c.receiveLocks.LoadOrStore(repo, newRepositoryLock())
	return value.(*repositoryLock)
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
	return c.RemoveRepositoryContext(context.Background(), input)
}
func (c *Cache) RemoveRepositoryContext(ctx context.Context, input string) error {
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	return c.removeRepositoryPath(ctx, path, nil)
}

// RemoveLegacyRepositoryContext uses the same path builder and locks for a
// flat pre-migration cache. The caller must prove no surviving mapping owns it.
func (c *Cache) RemoveLegacyRepositoryContext(ctx context.Context, input string) error {
	repo, err := NormalizeRepo(input)
	if err != nil {
		return err
	}
	return c.removeRepositoryPath(ctx, c.qualifiedCachePath("", "", repo), nil)
}

// RemoveRepositoryWith commits a catalog change while the live cache owner
// holds both receive and repository locks. Pending receive evidence is retained.
func (c *Cache) RemoveRepositoryWith(ctx context.Context, input string, change func() error) error {
	_, path, err := c.path(input)
	if err != nil {
		return err
	}
	return c.removeRepositoryPath(ctx, path, func() error {
		_, _, repo, err := ParseRepoPath(input)
		if err != nil {
			return err
		}
		if _, err := c.readReservation(c.reservationIdentity(repo, path)); err == nil {
			return ErrArchivePendingReceive
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return change()
	})
}
func (c *Cache) removeRepositoryPath(ctx context.Context, path string, change func() error) error {
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
	if change != nil {
		if err := change(); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(path); err != nil {
		return fmt.Errorf("remove repository cache after catalog removal: %w", err)
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
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return "", err
	}
	defer lock.RUnlock()
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
	_, path, err := c.path(input)
	if err != nil {
		return nil, err
	}
	lock := c.repoLock(path)
	if err := lock.RLockContext(ctx); err != nil {
		return nil, err
	}
	defer lock.RUnlock()
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
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := append([]string{"for-each-ref", "--format=%(refname) %(objectname)"}, prefixes...)
	buf := &boundedio.Buffer{Limit: maxRefsOutput, OnOverflow: cancel}
	err := c.run(ctx, commandSpec{dir: path, args: args, stdout: buf, stderr: buf})
	if buf.Err() != nil {
		return nil, fmt.Errorf("ref output exceeds %d bytes; too many refs", maxRefsOutput)
	}
	if err != nil {
		return nil, err
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
	lock := c.repoLock(path)
	if lock.TryLock() {
		if err := c.cleanReplacementRefsLocked(ctx, path); err != nil {
			lock.Unlock()
			return err
		}
		lock.downgrade()
	} else if err := lock.RLockContext(ctx); err != nil {
		return err
	}
	defer lock.RUnlock()
	if err := c.checkReceiveOutbox(input); err != nil {
		return err
	}
	if !c.isBare(ctx, path) {
		return fmt.Errorf("repository %s is not cached", repo)
	}
	return c.serveLocked(ctx, path, parsed, protocol, stdin, stdout, stderr)
}

func (c *Cache) serveLocked(ctx context.Context, path string, service Service, protocol string, stdin io.Reader, stdout, stderr io.Writer) error {
	extra := map[string]string{}
	if protocol != "" {
		extra["GIT_PROTOCOL"] = protocol
	}
	return c.runTransport(ctx, commandSpec{
		dir: path, args: c.serviceArgs(service, path),
		stdin: stdin, stdout: stdout, stderr: stderr, env: extra,
	})
}

func (c *Cache) runTransport(ctx context.Context, command commandSpec) error {
	// Repository preparation precedes this point; durable finalization follows it.
	if transport, ok := command.stdin.(interface {
		BeginTransport()
		EndTransport()
	}); ok {
		transport.BeginTransport()
		defer transport.EndTransport()
	}
	return c.run(ctx, command)
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
			"-c", "receive.advertisePushOptions=true",
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

// CacheRoot identifies the daemon-owned repository cache for removal requests.
func (c *Cache) CacheRoot() string { return c.root }

// missingRepositoryHint names the repository path the forge refused and, for
// an absolute ssh:// path, the home-relative upstream form that plain
// git-shell forges need. The first clone is where a wrong organization path
// surfaces, so the error must say how to correct the registration.
func (c *Cache) missingRepositoryHint(ctx context.Context, path string, err error) string {
	if err == nil || !strings.Contains(err.Error(), "does not appear to be a git repository") {
		return ""
	}
	remote, getErr := c.capture(ctx, path, "remote", "get-url", "upstream")
	if getErr != nil {
		return ""
	}
	parsed, parseErr := url.Parse(strings.TrimSpace(remote))
	if parseErr != nil || parsed.Scheme != "ssh" {
		return ""
	}
	hint := "; the forge has no repository at " + parsed.Path
	if !strings.HasPrefix(parsed.Path, "/~") {
		base := *parsed
		base.Path = "/~" + pathpkg.Dir(parsed.Path)
		hint += "; if it keeps repositories relative to the git user's home, register the upstream as " + base.String() + " (upstream remove, then upstream add)"
	}
	return hint
}
