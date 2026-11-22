package gitcache

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// ReceiveSource reads native quarantine objects only for the gate's lifetime.
// Other repositories use their ordinary exact source, whose refresh (a pin's
// producer head) may move their refs as any read does; nothing is imported
// into the pushed repository and none of its refs moves. C3 owns which
// revisions and policies to inspect.
type ReceiveSource struct {
	base, pushed *Cache
	ctx          context.Context
	event        ReceiveEvent
	// delivering records that this receive began putting its release tag on
	// the forge; delivered, that the tag is there.
	delivering, delivered atomic.Bool
}

func (
	c *Cache,
) receiveSource(
	ctx context.Context,
	store string,
	record Reservation,
	quarantine string,
) (
	*ReceiveSource,
	error,
) {
	objects := filepath.Join(store, "objects")
	quarantine = filepath.Clean(quarantine)
	if !filepath.IsAbs(quarantine) ||
		filepath.Dir(quarantine) != objects ||
		!strings.HasPrefix(filepath.Base(quarantine), "tmp_objdir-incoming-") {
		return nil, refuse("receive quarantine")
	}
	info, err := os.Lstat(quarantine)
	if err != nil || !info.IsDir() {
		return nil, refuse("receive quarantine")
	}
	env := []string{"GIT_OBJECT_DIRECTORY=" + quarantine, "GIT_ALTERNATE_OBJECT_DIRECTORIES=" + strconv.Quote(objects)}
	scoped := &Cache{
		ctx:       c.ctx,
		budgets:   c.budgets,
		transfers: c.transfers,
		unscoped:  c,
		config:    c.config,
		gates:     c.gates,
		rootLock:  c.rootLock,
		export:    c.export,
		activeMu:  c.activeMu,
		active:    c.active,
		objectEnv: env,
	}
	return &ReceiveSource{base: c, pushed: scoped, ctx: ctx, event: record.Event}, nil
}
func (s *ReceiveSource) scope(ctx context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	if s.ctx.Err() != nil {
		cancel()
	}
	return ctx, func() { stop(); cancel() }
}
func (s *ReceiveSource) Pin(ctx context.Context, at sourcecontract.Revision) (int64, bool, error) {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	cache := s.base
	if at.Repository == s.event.Repository {
		cache = s.pushed
	}
	return cache.Pin(ctx, at)
}
func (s *ReceiveSource) ReadFile(ctx context.Context, at gitport.Revision, path string) (gitport.File, error) {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	cache := s.base
	if at.Repository == s.event.Repository {
		cache = s.pushed
	}
	return cache.ReadFile(ctx, at, path)
}
func (
	s *ReceiveSource,
) Checkout(
	ctx context.Context,
	repo string,
	ref sourcecontract.Ref,
) (
	sourcecontract.Export,
	error,
) {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	if repo != s.event.Repository {
		return s.base.Checkout(ctx, repo, ref)
	}
	if ref == (sourcecontract.Ref{}) {
		return s.pushed.Checkout(ctx, repo, ref)
	}
	r := checkoutRequest{repository: repo, ref: ref, updates: s.event.Updates}
	if s.event.Admission.SHA != "" {
		r.defaultHead = sourcecontract.Ref{
			Name:      s.event.Admission.DefaultBranch,
			ObjectSHA: s.event.Admission.SHA,
			CommitSHA: s.event.Admission.SHA,
		}
	}
	return s.pushed.checkout(ctx, r)
}

type checkoutRequest struct {
	repository       string
	ref, defaultHead sourcecontract.Ref
	updates          []RefUpdate
}

// Checkout records the exact public inventory used by ordinary admission.
// ReceiveSource supplies the corresponding pre-mutation quarantine view.
func (c *Cache) Checkout(ctx context.Context, repo string, ref sourcecontract.Ref) (sourcecontract.Export, error) {
	if ref != (sourcecontract.Ref{}) {
		if err := ref.Validate(); err != nil {
			return sourcecontract.Export{}, err
		}
	}
	if _, err := c.path(repo); err != nil {
		return sourcecontract.Export{}, err
	}
	remote, err := c.remote(ctx, repo)
	if err != nil {
		return sourcecontract.Export{}, err
	}
	var head gitport.Ref
	if ref == (sourcecontract.Ref{}) {
		head, err = c.RemoteRef(ctx, repo, remote.DefaultBranch)
	} else {
		head, err = c.ResolveRef(ctx, repo, remote.DefaultBranch)
	}
	if err != nil && (ref == (sourcecontract.Ref{}) || !isAbsent(err)) {
		return sourcecontract.Export{}, err
	}
	r := checkoutRequest{repository: repo, ref: ref}
	if err == nil {
		r.defaultHead = sourcecontract.Ref(head)
		if ref == (sourcecontract.Ref{}) {
			r.ref = r.defaultHead
		}
	}
	return c.checkout(ctx, r)
}
func (c *Cache) checkout(ctx context.Context, r checkoutRequest) (sourcecontract.Export, error) {
	if err := r.ref.Validate(); err != nil {
		return sourcecontract.Export{}, err
	}
	p, err := c.path(r.repository)
	if err != nil {
		return sourcecontract.Export{}, err
	}
	if err = c.lockRead(ctx, p); err != nil {
		return sourcecontract.Export{}, err
	}
	defer c.unlock(p)
	r.ref, err = c.checkoutRef(ctx, p, r.ref)
	if err != nil {
		return sourcecontract.Export{}, err
	}
	refs, err := c.list(ctx, p, r.repository)
	if err != nil {
		return sourcecontract.Export{}, err
	}
	tags := make(map[string]sourcecontract.Ref)
	for _, ref := range refs {
		if strings.HasPrefix(ref.Name, "refs/tags/") {
			tags[ref.Name] = sourcecontract.Ref(ref)
		}
	}
	if err = c.pendingTags(ctx, p, tags, r.updates); err != nil {
		return sourcecontract.Export{}, err
	}
	export := sourcecontract.Export{Ref: r.ref, DefaultBranch: r.defaultHead}
	for _, tag := range tags {
		export.Tags = append(export.Tags, tag)
	}
	slices.SortFunc(export.Tags, func(a, b sourcecontract.Ref) int { return strings.Compare(a.Name, b.Name) })
	return export, export.Validate()
}
func (
	c *Cache,
) pendingTags(
	ctx context.Context,
	p string,
	tags map[string]sourcecontract.Ref,
	updates []RefUpdate,
) error {
	for _, u := range updates {
		if !strings.HasPrefix(u.Ref, "refs/tags/") {
			continue
		}
		if u.NewSHA == "" {
			delete(tags, u.Ref)
			continue
		}
		ref, err := c.peelRemote(ctx, p, gitport.Ref{Name: u.Ref, ObjectSHA: u.NewSHA})
		if err != nil {
			return err
		}
		tags[u.Ref] = sourcecontract.Ref(ref)
	}
	return nil
}

func (c *Cache) checkoutRef(ctx context.Context, p string, ref sourcecontract.Ref) (sourcecontract.Ref, error) {
	if err := c.object(ctx, p, ref.ObjectSHA); err != nil {
		return ref, err
	}
	resolved, err := c.peelRemote(ctx, p, gitport.Ref(ref))
	if err != nil {
		return ref, err
	}
	if ref.CommitSHA != "" && ref.CommitSHA != resolved.CommitSHA {
		return ref, refuse("checkout commit")
	}
	ref.CommitSHA = resolved.CommitSHA
	return ref, nil
}
