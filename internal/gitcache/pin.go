package gitcache

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// headDeadline is how long a pin or a release's ancestry check waits for the
// forge's default head before it decides from the head the cache holds. One
// forge round trip fits; a slower read continues in the background, and
// several slow producers still fit in a push's 30-second admission and in a
// rerun's 10-second request bound.
const headDeadline = 3 * time.Second

// Pin reads producer identity and fresh ancestry; it creates no retention refs.
// ReceiveSource uses the same read in quarantine without importing pushed data.
func (c *Cache) Pin(ctx context.Context, at sourcecontract.Revision) (int64, bool, error) {
	id, _, reachable, err := c.defaultAncestor(ctx, at)
	return id, reachable, err
}

// DefaultAncestor returns the exact default head used for the ancestry decision.
func (c *Cache) DefaultAncestor(ctx context.Context, at sourcecontract.Revision) (sourcecontract.Ref, bool, error) {
	_, head, reachable, err := c.defaultAncestor(ctx, at)
	return head, reachable, err
}

// defaultAncestor decides from the forge's default head (admissionHead). Only
// a head read that gave no head leaves the decision to the head the cache
// holds, and only for a commit that head reaches.
func (c *Cache) defaultAncestor(ctx context.Context, at sourcecontract.Revision) (int64, sourcecontract.Ref, bool, error) {
	r, p, err := c.pinProducer(ctx, at)
	if err != nil || r.RepositoryID == 0 {
		return 0, sourcecontract.Ref{}, false, err
	}
	head, late, err := c.admissionHead(ctx, p, r)
	if late != nil {
		if held, reachable := c.heldAncestor(ctx, p, r.DefaultBranch, at.Commit); reachable {
			return r.RepositoryID, held, true, nil
		}
		return r.RepositoryID, sourcecontract.Ref{}, false, forgeRefusal(reasonForgeSilent, remedyForgeSilent, at.Repository, late)
	}
	if isAbsent(err) {
		return r.RepositoryID, sourcecontract.Ref{}, false, nil
	}
	if err != nil {
		return r.RepositoryID, sourcecontract.Ref{}, false, err
	}
	reachable, err := c.reaches(ctx, p, at.Commit, head.CommitSHA)
	return r.RepositoryID, head, reachable, err
}

// ownPin decides a pin of the pushed repository's own commit during its own
// admission from the receive and the cache, never the forge: the commit is
// reachable from the commit this receive moves the default branch to, or from
// the default head the cache holds while the cache was refreshed from the
// forge within cacheTrustWindow (each push refreshes its repository first);
// with an older cache, during an outage of its forge, it is refused naming
// that forge. A commit that reached the forge outside Oberth since the last
// refresh is not reachable yet: that pin is refused until the cache sees it.
func (c *Cache) ownPin(ctx context.Context, at sourcecontract.Revision, updates []RefUpdate) (int64, bool, error) {
	r, p, err := c.pinProducer(ctx, at)
	if err != nil || r.RepositoryID == 0 {
		return 0, false, err
	}
	for _, u := range updates {
		if u.Ref != r.DefaultBranch || u.NewSHA == "" {
			continue
		}
		if reachable, err := c.reaches(ctx, p, at.Commit, u.NewSHA); err != nil || reachable {
			return r.RepositoryID, reachable, err
		}
	}
	if _, reachable := c.heldAncestor(ctx, p, r.DefaultBranch, at.Commit); !reachable {
		return r.RepositoryID, false, nil
	}
	if !c.trustedCache(p) {
		return r.RepositoryID, false, forgeRefusal(reasonForgeSilent, remedyForgeSilent, at.Repository, nil)
	}
	return r.RepositoryID, true, nil
}

func (c *Cache) pinProducer(ctx context.Context, at sourcecontract.Revision) (Remote, string, error) {
	if err := at.Validate(); err != nil {
		return Remote{}, "", err
	}
	if err := ctx.Err(); err != nil {
		return Remote{}, "", context.Cause(ctx)
	}
	r, err := c.producer(ctx, at.Repository)
	if err != nil || r.RepositoryID == 0 {
		return Remote{}, "", err
	}
	p, err := c.path(at.Repository)
	return r, p, err
}

// heldAncestor reports whether commit is reachable from the default head the
// cache holds, and that head.
func (c *Cache) heldAncestor(ctx context.Context, p, branch, commit string) (sourcecontract.Ref, bool) {
	head, err := c.trackedHead(ctx, p, branch)
	if err != nil {
		return sourcecontract.Ref{}, false
	}
	reachable, err := c.reaches(ctx, p, commit, head.CommitSHA)
	return head, err == nil && reachable
}

// trackedHead is the forge's default head as the cache last saw it: its
// tracking ref, which the first clone, a refresh and a delivery move.
func (c *Cache) trackedHead(ctx context.Context, p, branch string) (gitport.Ref, error) {
	head, err := c.resolve(ctx, p, "", trackingName(branch))
	if err == nil && head.CommitSHA == "" {
		err = refuse("branch commit")
	}
	head.Name = branch
	return head, err
}

// trackingName is the ref that holds public ref name's last upstream value.
func trackingName(name string) string {
	return "refs/oberth/upstream/" + strings.TrimPrefix(name, "refs/")
}

// reaches reports whether commit is in the cache and reachable from head.
func (c *Cache) reaches(ctx context.Context, p, commit, head string) (bool, error) {
	if err := c.commit(ctx, p, commit); isAbsent(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	return c.ancestor(ctx, p, commit, head)
}

func (c *Cache) producer(ctx context.Context, repo string) (Remote, error) {
	if c.config.Upstream == nil {
		return Remote{}, nil
	}
	r, err := c.config.Upstream(ctx, repo)
	if err != nil {
		return Remote{}, unavailable("read producer registration", err)
	}
	if r.RepositoryID == 0 {
		return Remote{}, nil
	}
	if r.RepositoryID < 0 {
		return Remote{}, refuse("producer registration")
	}
	r.repository = repo
	r, err = validateRemote(r)
	if err != nil {
		return r, err
	}
	return r, nil
}

func (c *Cache) ancestor(ctx context.Context, p, commit, head string) (bool, error) {
	err := c.run(ctx, command{dir: p, args: []string{"merge-base", "--is-ancestor", commit, head}})
	var exit *exec.ExitError
	if errors.As(err, &exit) && exit.ExitCode() == 1 {
		return false, nil
	}
	return err == nil, err
}
