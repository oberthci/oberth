package gitcache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/reponame"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"k8s.io/utils/clock"
)

// Config contains operator paths and injected repository configuration. Binary
// is the packaged static Git executable; tests supply a real local Git binary.
type Config struct {
	// DeliveryContext is the server lifetime, separate from construction or push contexts.
	// Standalone caches own their delivery lifetime until Close.
	DeliveryContext  context.Context
	MaxInputBytes    int64
	ReservedBranches []string
	Root             string
	Scratch          string
	Binary           string
	HookBinary       string
	RefChanged       func(context.Context, string, []RefUpdate)
	// PreReceive admits run-requesting pushes before refs move. It must honor
	// its request context. C3 supplies the admission duration and public errors.
	PreReceive func(context.Context, ReceiveEvent, *ReceiveSource) error
	// AdmissionTimeout bounds admission and accepted receive delivery.
	// Composition supplies its admission duration; zero uses thirty seconds.
	AdmissionTimeout time.Duration
	// AdmissionMetric observes a callback still running at cancellation, once
	// per receive. Observers must be nonblocking.
	AdmissionMetric func(reason string)
	// Log carries cache observations and native operation failure details.
	// It must be nonblocking; detail belongs in the server log, not feedback.
	Log     func(reason, detail string)
	Observe func(sourcedelivery.Summary)
	Permits *sourcedelivery.Permits
	Now     func() time.Time
	// AbortReceive forgets a started admission that never reaches Deliver.
	// It is nonblocking and performs no I/O; refusing/cancelled attempts may
	// have no saved preparation, so forgetting an unknown ID must be harmless.
	AbortReceive func(receiveID string)
	Upstream     func(context.Context, string) (Remote, error)
	LiveOwners   func(context.Context, string) (OwnerPage, error)
}

// Remote selects exactly one registered endpoint and its dedicated SSH command.
// Local absolute paths support offline repositories; network endpoints use SSH.
type Remote struct {
	URL, SSHBinary, SSHConfig, DefaultBranch string
	RepositoryID                             int64
	// repository is the canonical name the cache read the remote for; forge
	// failures in the server log name it.
	repository string
}

type Cache struct {
	ctx            context.Context
	stopDelivery   context.CancelFunc
	config         Config
	gates          *repositoryGates
	rootLock       *os.File
	export         chan struct{}
	staged         *stagedArchives
	activeMu       *sync.Mutex
	active         map[string]bool
	receives       map[string]*receiveFiles
	replayProblems map[string]string
	objectEnv      []string
	budgets        gitBudgets
	transfers      *transfers
	// answers keeps each forge identity's last default head answer.
	answers *headAnswers
	// unscoped is the cache a receive-scoped copy was made from; transfers
	// that outlive the receive run there, never in its quarantine.
	unscoped     *Cache
	hookIdentity *trustedHook
	// Receive boundaries permit deterministic fault injection.
	receivePrepared, receiveReturned func()
}

func New(ctx context.Context, config Config) (*Cache, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if config.Now == nil {
		config.Now = clock.RealClock{}.Now
	}
	config.ReservedBranches = slices.Clone(config.ReservedBranches)
	if err := prepareCachePaths(config); err != nil {
		return nil, err
	}
	owner, err := ownRoot(config.Root)
	if err != nil {
		return nil, err
	}
	opened := false
	defer func() {
		if !opened {
			_ = owner.Close()
		}
	}()
	if err := writeManagedConfig(&config); err != nil {
		return nil, err
	}
	if err := cleanMaterializations(config.Root); err != nil {
		return nil, err
	}
	if err := cleanExports(config.Scratch); err != nil {
		return nil, unavailable("clean source scratch", err)
	}
	if config.AdmissionTimeout <= 0 {
		config.AdmissionTimeout = 30 * time.Second
	}
	cache := &Cache{
		budgets:   defaultGitBudgets(),
		transfers: &transfers{running: map[string]*transfer{}},
		answers:   newHeadAnswers(),
		config:    config,
		rootLock:  owner,
		gates:     &repositoryGates{repos: make(map[string]chan struct{})},
		export:    make(chan struct{}, 1),
		staged:    newStagedArchives(),
		activeMu:  &sync.Mutex{},
		active:    make(map[string]bool),
	}
	if err := cache.recoverMirrors(ctx); err != nil {
		return nil, err
	}
	if err := cache.loadHookIdentity(); err != nil {
		return nil, err
	}
	cache.startDelivery(ctx)
	opened = true
	return cache, nil
}
func prepareCachePaths(config Config) error {
	if err := validatePatterns(config.ReservedBranches); err != nil {
		return err
	}
	if !filepath.IsAbs(config.Root) || !filepath.IsAbs(config.Scratch) || !filepath.IsAbs(config.Binary) {
		return refuse("configuration")
	}
	for _, dir := range []string{config.Root, config.Scratch} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			return unavailable("initialize", err)
		}
	}
	return nil
}

// Close releases root ownership after the caller has stopped all requests;
// it ends running forge transfers and waits for them before releasing it.
func (c *Cache) Close() error {
	c.stopDelivery()
	c.transfers.close()
	return errors.Join(c.staged.prune(nil), c.rootLock.Close())
}

func (c *Cache) path(repository string) (string, error) {
	r, err := reponame.ParseRepository(repository)
	if err != nil {
		return "", refuse("repository")
	}
	return filepath.Join(c.config.Root, r.Upstream, r.Organization, r.Name+".git"), nil
}

// Init materializes only the canonical bare layout. A first clone from the
// upstream outlives a caller deadline shorter than it needs (awaitClone).
func (c *Cache) Init(ctx context.Context, repository string) error {
	p, err := c.path(repository)
	if err != nil {
		return err
	}
	ready, err := c.initialized(ctx, p)
	if err != nil || ready {
		return err
	}
	if c.config.Upstream != nil {
		return c.awaitClone(ctx, repository, p)
	}
	slot := c.repositoryGate(p + "/init")
	if err = acquireContext(ctx, slot); err != nil {
		return err
	}
	defer func() { <-slot }()
	if _, err = os.Stat(filepath.Join(p, "HEAD")); err == nil {
		return nil
	}
	staging := p + ".staging"
	if err = os.RemoveAll(staging); err != nil {
		return err
	}
	if err = os.MkdirAll(staging, 0700); err != nil {
		return unavailable("initialize", err)
	}
	if _, err = c.capture(
		ctx,
		command{dir: staging, args: []string{"init", "--bare", "--initial-branch=main", "."}},
		1<<20,
	); err != nil {
		return err
	}
	return c.publishClone(ctx, staging, p)
}

// initialized reports whether p holds the bare layout and checks it when so.
func (c *Cache) initialized(ctx context.Context, p string) (bool, error) {
	slot := c.repositoryGate(p + "/init")
	if err := acquireContext(ctx, slot); err != nil {
		return false, err
	}
	defer func() { <-slot }()
	if _, err := os.Stat(filepath.Join(p, "HEAD")); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, unavailable("initialize", err)
	}
	if err := c.lock(ctx, p); err != nil {
		return true, err
	}
	defer c.unlock(p)
	_, err := c.capture(ctx, command{dir: p, args: []string{"rev-parse", "--is-bare-repository"}}, 64)
	return true, err
}

func (c *Cache) clone(ctx context.Context, repository, p string) error {
	remote, err := c.remote(ctx, repository)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		return err
	}
	staging := p + ".staging"
	if err = os.RemoveAll(staging); err != nil {
		return err
	}
	if err = c.run(
		ctx,
		command{
			dir: c.config.Root,
			args: []string{
				"clone",
				"--progress",
				"--bare",
				"--no-local",
				"--no-hardlinks",
				"--",
				remote.URL,
				staging,
			},
			remote: &remote,
		},
	); err != nil {
		return err
	}
	if err = c.seedTracking(ctx, staging, repository); err != nil {
		return err
	}
	var empty *gitport.EmptyRepositoryError
	if err = c.checkDefault(ctx, staging, repository, remote); err != nil && !errors.As(err, &empty) {
		return err
	}
	if err = c.recordRefresh(staging, remote); err != nil {
		return err
	}
	// The upstream can be removed and added again under another address while
	// a long first clone runs: the old forge's content is not published.
	if current, err := c.remote(ctx, repository); err != nil {
		return errors.Join(unavailable("initialize", errUpstreamUnread), err)
	} else if current.URL != remote.URL {
		return unavailable("initialize", errUpstreamMoved)
	}
	return c.publishClone(ctx, staging, p)
}

// errUpstreamMoved and errUpstreamUnread say why a finished transfer was not
// published: the upstream changed address, or its registration could not be
// read again to tell.
var (
	errUpstreamMoved  = errors.New("the upstream address changed during the transfer")
	errUpstreamUnread = errors.New("the upstream registration could not be read after the transfer")
)

func refuse(field string) error {
	reason, remedy := refusalText(field)
	return &RefusalError{reason: reason, remedy: remedy, Field: field}
}
func unavailable(op string, err error) error { return &gitport.UnavailableError{Op: op, Cause: err} }

// ValidOwner is the Git keep-ref owner grammar shared with the in-memory port.
func ValidOwner(owner string) bool {
	if len(owner) < 1 || len(owner) > 128 || strings.HasPrefix(owner, ".") {
		return false
	}
	for _, r := range owner {
		if !ownerCharacter(r) {
			return false
		}
	}
	return true
}

func ownerCharacter(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_'
}

func validatePatterns(patterns []string) error {
	for _, pattern := range patterns {
		if len(pattern) > 255 {
			return refuse("reserved branch pattern")
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return refuse("reserved branch pattern")
		}
	}
	return nil
}

func (c *Cache) publishClone(ctx context.Context, staging, p string) error {
	if err := c.lock(ctx, p); err != nil {
		return err
	}
	defer c.unlock(p)
	return os.Rename(staging, p)
}

func writeManagedConfig(config *Config) error {
	if config.MaxInputBytes == 0 {
		config.MaxInputBytes = 1 << 30
	}
	if config.MaxInputBytes < 0 {
		return refuse("receive pack size")
	}
	managed := fmt.Sprintf(managedGitConfig, config.MaxInputBytes)
	return os.WriteFile(filepath.Join(config.Root, "oberth.gitconfig"), []byte(managed), 0600)
}

const managedGitConfig = `[gc]
 autoDetach=false
[maintenance]
 autoDetach=false
[transfer]
 fsckObjects=true
 hideRefs=refs/oberth/
[receive]
 maxInputSize=%d
 autogc=false
 fsckObjects=true
[fetch]
 fsckObjects=true
`
