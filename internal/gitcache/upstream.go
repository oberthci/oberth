package gitcache

import (
	"context"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (c *Cache) remote(ctx context.Context, repo string) (Remote, error) {
	p, err := c.path(repo)
	if err != nil {
		return Remote{}, err
	}
	if err = c.lock(ctx, p); err != nil {
		return Remote{}, err
	}
	c.unlock(p)
	if c.config.Upstream == nil {
		return Remote{}, refuse("upstream")
	}
	r, err := c.config.Upstream(ctx, repo)
	if err != nil {
		return r, unavailable("read upstream configuration", err)
	}
	r.repository = repo
	return validateRemote(r)
}
func validateRemote(r Remote) (Remote, error) {
	if sourcecontract.ValidatePublicRef(r.DefaultBranch) != nil || !strings.HasPrefix(r.DefaultBranch, "refs/heads/") {
		return r, refuse("default branch")
	}
	if filepath.IsAbs(r.URL) && filepath.Clean(r.URL) == r.URL {
		return r, nil
	}
	return r, validateRemoteSSH(r)
}
func validateRemoteSSH(r Remote) error {
	u, err := url.Parse(r.URL)
	if err != nil {
		return refuse("upstream URL")
	}
	if u.Scheme != "ssh" || u.Hostname() == "" || u.User == nil || u.RawQuery != "" || u.Fragment != "" {
		return refuse("upstream URL")
	}
	if sourcecontract.ValidateFilePath(strings.TrimPrefix(u.Path, "/")) != nil {
		return refuse("upstream URL path")
	}
	if _, ok := u.User.Password(); ok {
		return refuse("upstream credentials")
	}
	if !filepath.IsAbs(r.SSHBinary) || !filepath.IsAbs(r.SSHConfig) {
		return refuse("upstream SSH identity")
	}
	return nil
}

func (c *Cache) RemoteRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if ref != "HEAD" {
		if err := sourcecontract.ValidatePublicRef(ref); err != nil {
			return gitport.Ref{}, err
		}
	}
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	if err := c.refresh(ctx, repo, false); err != nil {
		return gitport.Ref{}, err
	}
	r, err := c.remote(ctx, repo)
	if err != nil {
		return gitport.Ref{}, err
	}

	if ref == r.DefaultBranch {
		return c.policyHead(ctx, repo, r)
	}
	head, err := c.remoteRef(ctx, p, r, ref)
	if ref == "HEAD" && isAbsent(err) {
		return head, c.policyAbsence(ctx, p, r)
	}
	if err != nil {
		return head, err
	}
	if err = c.fetchObject(ctx, p, r, fetchedObject{oid: head.ObjectSHA}); err != nil {
		return head, err
	}
	return c.peelRemote(ctx, p, head)
}
func (c *Cache) remoteRef(ctx context.Context, p string, r Remote, ref string) (gitport.Ref, error) {
	data, err := c.capture(
		ctx,
		command{
			dir:    p,
			args:   []string{"ls-remote", "--", r.URL, ref, ref + "^{}"},
			remote: &r,
		},
		1<<20,
	)
	if err != nil {
		return gitport.Ref{}, err
	}
	var result gitport.Ref
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		if f[1] == ref {
			result.Name = ref
			result.ObjectSHA = f[0]
		}
		if f[1] == ref+"^{}" {
			result.CommitSHA = f[0]
		}
	}
	if result.Name == "" {
		return result, &gitport.AbsentError{Ref: ref}
	}
	if strings.HasPrefix(ref, "refs/heads/") {
		result.CommitSHA = result.ObjectSHA
	}
	return result, nil
}

// Fetch refreshes stale forge refs and notifies their readers after releasing
// the repository lock. Accepted local heads are preserved.
func (c *Cache) Fetch(ctx context.Context, repo string) error {
	return c.refresh(ctx, repo, false)
}

// fetchTarget is one refresh fetch: the repository, its store and remote, and
// the flag its forge command sets once the forge is sending a pack.
type fetchTarget struct {
	store, repo string
	remote      Remote
	sending     *atomic.Bool
}

func (c *Cache) fetch(ctx context.Context, t fetchTarget) (updates []RefUpdate, result error) {
	p, repo, r := t.store, t.repo, t.remote
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-fetch-")
	if err != nil {
		return nil, err
	}
	defer func() { result = errors.Join(result, os.RemoveAll(dir)) }()
	after, err := c.fetchSnapshot(ctx, dir, t)
	if err != nil {
		return nil, err
	}
	if len(after) == 0 {
		return nil, &gitport.EmptyRepositoryError{}
	}
	if err := c.sameUpstream(ctx, repo, r); err != nil {
		return nil, err
	}
	if err := c.lock(ctx, p); err != nil {
		return nil, err
	}
	defer c.unlock(p)
	if receivePending(p) {
		return nil, refuse("pending receive")
	}
	before, err := c.tracking(ctx, p)
	if err != nil {
		return nil, err
	}
	args := []string{"fetch", "--no-auto-maintenance", "--no-write-fetch-head", "--no-tags", "--", dir}
	for _, oid := range after {
		args = append(args, oid)
	}
	if len(after) > 0 {
		if err = c.run(ctx, command{dir: p, args: args}); err != nil {
			return nil, err
		}
	}
	return c.mirror(ctx, p, mirrorChange{before: before, after: after, repository: repo})
}

// sameUpstream refuses to import a fetch that outlived its upstream removed
// and added again under another address or catalogue ID: the old forge's
// content stays out, as it does for a first clone.
func (c *Cache) sameUpstream(ctx context.Context, repo string, r Remote) error {
	current, err := c.remote(ctx, repo)
	if err != nil {
		return errors.Join(unavailable("refresh", errUpstreamUnread), err)
	}
	if current.URL != r.URL || r.RepositoryID != 0 && current.RepositoryID != r.RepositoryID {
		return unavailable("refresh", errUpstreamMoved)
	}
	return nil
}

func (c *Cache) fetchSnapshot(ctx context.Context, dir string, t fetchTarget) (map[string]string, error) {
	store, r := t.store, t.remote
	format, err := c.capture(ctx, command{dir: store, args: []string{"rev-parse", "--show-object-format"}}, 64)
	if err != nil {
		return nil, err
	}
	if err = c.run(
		ctx,
		command{
			dir:  dir,
			args: []string{"init", "--bare", "--object-format=" + string(format), "."},
		},
	); err != nil {
		return nil, err
	}
	if err = os.WriteFile(
		filepath.Join(dir, "objects", "info", "alternates"),
		[]byte(strconv.Quote(filepath.Join(store, "objects"))+"\n"),
		0600,
	); err != nil {
		return nil, err
	}
	args := []string{
		"fetch",
		"--progress",
		"--no-auto-maintenance",
		"--no-write-fetch-head",
		"--no-tags",
		"--",
		r.URL,
		"+refs/heads/*:refs/heads/*",
		"+refs/tags/*:refs/tags/*",
	}
	if err = c.run(ctx, command{dir: dir, args: args, remote: &r, sending: t.sending}); err != nil {
		return nil, err
	}
	return c.trackingAt(ctx, dir, "refs/", sourcecontract.MaxRefs)
}

func (c *Cache) changed(ctx context.Context, repo string, updates []RefUpdate) {
	if len(updates) > 0 && c.config.RefChanged != nil {
		c.config.RefChanged(ctx, repo, updates)
	}
}

// fetchedObject is one object a fetch brings, and the flag its forge command
// raises once the forge is sending (nil when no caller reads it).
type fetchedObject struct {
	oid     string
	sending *atomic.Bool
}

func (c *Cache) fetchObject(ctx context.Context, p string, r Remote, o fetchedObject) error {
	return c.run(
		ctx,
		command{
			dir: p,
			args: []string{
				"fetch",
				"--progress",
				"--no-auto-maintenance",
				"--no-tags",
				"--no-write-fetch-head",
				"--",
				r.URL,
				o.oid,
			},
			remote:  &r,
			sending: o.sending,
		},
	)
}
func (c *Cache) peelRemote(ctx context.Context, p string, head gitport.Ref) (gitport.Ref, error) {
	data, err := c.capture(
		ctx,
		command{
			dir:   p,
			args:  []string{"cat-file", "--batch-check=%(objectname) %(objecttype)"},
			input: strings.NewReader(head.ObjectSHA + "^{commit}\n"),
		},
		256,
	)
	if err != nil {
		return head, err
	}
	f := strings.Fields(string(data))
	if len(f) != 2 {
		return head, refuse("upstream ref object")
	}
	if f[1] == "commit" {
		head.CommitSHA = f[0]
	}
	return head, nil
}
