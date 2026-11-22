package gitcache

import (
	"context"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/gitssh"
	"github.com/oberthci/oberth/internal/publicerr"
)

// cacheTrustWindow is how recently the cache must have been refreshed from a
// forge for its default head to decide an admission read the forge was too
// slow or unreachable to answer: a commit removed from the default branch
// upstream stays admissible at most this long while the forge is away.
const cacheTrustWindow = 15 * time.Minute

// forgeError reports whether err is a forge outcome: it did not answer in
// time, is still sending, or could not be read.
func forgeError(err error) bool {
	var outage *gitport.UnavailableError
	var updating *gitport.FetchUnfinishedError
	return errors.As(err, &updating) || errors.As(err, &outage) && outage.Op == "forge"
}

// forgeSlow reports whether a forge outcome says only that the forge was slow
// or unreachable: the caller stopped waiting, the copy is still updating, the
// command ran to its own bound, or the transport reached no server or lost
// it. A refused host key or identity, or a forge that answered with an
// error, is not slow.
func forgeSlow(err error) bool {
	var updating *gitport.FetchUnfinishedError
	if errors.As(err, &updating) {
		return true
	}
	var outage *gitport.UnavailableError
	if !errors.As(err, &outage) || outage.Op != "forge" || outage.Cause == nil {
		return false
	}
	return errors.Is(outage.Cause, context.DeadlineExceeded) || gitssh.TransportFailure(outage.Cause.Error())
}

// lateForge decides a forge outcome of r's head read: nil when the forge was
// slow and the cache at p was refreshed from it within cacheTrustWindow, so
// the cache may decide; otherwise the refusal that names r's repository.
func (c *Cache) lateForge(p string, r Remote, err error) error {
	if !forgeSlow(err) {
		return forgeRefusal(reasonForgeRefused, remedyForgeRefused, r.repository, err)
	}
	if !c.trustedCache(p, r) {
		return forgeRefusal(reasonForgeSilent, remedyForgeSilent, r.repository, err)
	}
	return nil
}

// trustedCache reports whether the cache at p was refreshed from r's forge
// (its address and catalogue ID) within cacheTrustWindow, so its default head
// may decide without the forge. A refresh from another address never counts,
// nor one recorded after now (a clock that stepped back).
func (c *Cache) trustedCache(p string, r Remote) bool {
	last := c.lastRefresh(p)
	if last.IsZero() || refreshedFrom(p) != forgeIdentity(r) {
		return false
	}
	age := c.config.Now().Sub(last)
	return age >= 0 && age <= cacheTrustWindow
}

// forgeRefusal refuses a head read naming repo's forge. A forge that answered
// and is still sending keeps saying so.
func forgeRefusal(reason, remedy, repo string, cause error) error {
	var updating *gitport.FetchUnfinishedError
	if errors.As(cause, &updating) {
		reason, remedy = gitport.FetchUnfinishedReason, gitport.FetchUnfinishedRemedy
	}
	refusal := publicerr.New(publicerr.Unavailable, reason, remedy, cause)
	refusal.Detail = "Forge of " + repo + "."
	return refusal
}

// DefaultHead reads the forge's default head of repo for a default-branch or
// tag push, under the pin rule: its forge reads are repository jobs, the
// caller waits at most the head wait, and a slow forge is accepted while the
// cache was refreshed from it within cacheTrustWindow.
func (c *Cache) DefaultHead(ctx context.Context, repo string) error {
	p, err := c.path(repo)
	if err != nil {
		return err
	}
	r, err := c.remote(ctx, repo)
	if err != nil {
		return err
	}
	_, _, err = c.admissionHead(ctx, p, r)
	return err
}

// admissionHead reads r's default head for an admission decision within the
// head wait. The forge's head decides when the forge answers, also while the
// transfer of that head still runs: that read is refused and its retry finds
// the transfer done. Only a head read that gave no head (late is the forge's
// outcome) leaves the decision to the cache, under lateForge's rule.
func (c *Cache) admissionHead(ctx context.Context, p string, r Remote) (head gitport.Ref, late, err error) {
	wait, cancel := context.WithTimeout(ctx, c.budgets.headWait)
	head, err = c.policyHead(wait, r.repository, r)
	cancel()
	switch {
	case !forgeError(err):
		return head, nil, err
	case head.ObjectSHA != "":
		return head, nil, forgeRefusal(reasonForgeSilent, remedyForgeSilent, r.repository, err)
	}
	if refusal := c.lateForge(p, r, err); refusal != nil {
		return gitport.Ref{}, nil, refusal
	}
	return gitport.Ref{}, err, nil
}
