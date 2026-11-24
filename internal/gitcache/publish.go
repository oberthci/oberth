package gitcache

import (
	"context"
	"errors"
	"os/exec"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// Publish settles one exact durable intent. Nil predecessor is unknown; empty
// predecessor means known absent. A retry reads back before moving anything.
func (c *Cache) Publish(ctx context.Context, r model.Publication) (gitport.PublicationReceipt, error) {
	if err := validatePublication(r); err != nil {
		return gitport.PublicationReceipt{}, err
	}
	p, err := c.path(r.Repo)
	if err != nil {
		return gitport.PublicationReceipt{}, err
	}
	remote, err := c.remote(ctx, r.Repo)
	if err != nil {
		return gitport.PublicationReceipt{}, err
	}
	if err = c.lock(ctx, p); err != nil {
		return gitport.PublicationReceipt{}, err
	}
	busy := c.publicationRefBusy(p, r.Ref)
	if !busy {
		c.setPublication(p+"\x00"+r.Ref, true)
	}
	c.unlock(p)
	if busy {
		return gitport.PublicationReceipt{}, unavailable("publish", errors.New(reasonPublishing))
	}
	defer c.setPublication(p+"\x00"+r.Ref, false)
	err = c.publishCached(ctx, p, remote, &r)
	if err != nil {
		return gitport.PublicationReceipt{}, err
	}
	if r.PromotionID != "" {
		if err = c.AbandonPromotion(ctx, r.Repo, r.PromotionID); err != nil {
			return gitport.PublicationReceipt{}, err
		}
	}
	ref := gitport.Ref{Name: r.Ref, ObjectSHA: r.ObjectSHA}
	if r.CommitSHA != nil {
		ref.CommitSHA = *r.CommitSHA
	}
	c.setPublication(p+"\x00"+r.Ref, false)
	c.changed(ctx, r.Repo, []RefUpdate{{Ref: r.Ref, OldSHA: *r.PreviousSHA, NewSHA: r.ObjectSHA}})
	return gitport.PublicationReceipt{Repository: r.Repo, ActionReceipt: r.ID, Refs: []gitport.Ref{ref}}, nil
}
func validatePublication(r model.Publication) error {
	if r.ID == "" || r.Actor == "" {
		return refuse("publication identity")
	}
	if err := sourcecontract.ValidatePublicRef(r.Ref); err != nil {
		return err
	}
	if r.PreviousSHA == nil {
		return refuse("publication predecessor")
	}
	objects := []string{r.ObjectSHA, *r.PreviousSHA}
	if len(r.ReceivedHeads) > model.MaxReceivedHeads {
		return refuse("publication receive heads")
	}
	objects = append(objects, r.ReceivedHeads...)
	for _, oid := range objects {
		if oid != "" && !gitoid.Valid(oid) {
			return refuse("publication object")
		}
	}
	if strings.HasPrefix(r.Ref, "refs/tags/") && (r.ObjectSHA == "" || *r.PreviousSHA != "") {
		return refuse("immutable tag")
	}
	return validatePublishedCommit(r)
}
func validatePublishedCommit(r model.Publication) error {
	if r.ObjectSHA == "" {
		return nil
	}
	if r.CommitSHA == nil || !gitoid.Valid(*r.CommitSHA) {
		return &RefusalError{reason: reasonPushCommit, remedy: remedyPushCommit}
	}
	if strings.HasPrefix(r.Ref, "refs/heads/") && *r.CommitSHA != r.ObjectSHA {
		return &RefusalError{reason: reasonPushCommit, remedy: remedyPushCommit}
	}
	return nil
}
func (c *Cache) publishIntent(ctx context.Context, p string, remote Remote, r *model.Publication) error {
	if err := c.publicationDefault(ctx, remote, *r); err != nil {
		return err
	}
	if r.Attempts > 1 || r.ReadBack {
		held, err := c.forgeHolds(ctx, p, remote, *r)
		if err != nil || held {
			return err
		}
	}
	if err := c.lock(ctx, p); err != nil {
		return err
	}
	err := c.checkIntent(ctx, p, *r, remote.DefaultBranch)
	c.unlock(p)
	if err != nil {
		return err
	}
	qualified, err := c.topicLease(ctx, p, remote, *r)
	if err != nil {
		return err
	}
	*r = qualified
	return c.pushIntent(ctx, p, remote, *r)
}

// A saved empty predecessor must not recreate an emptied established forge.
// Other refs always need a live default head, including on explicit retries.
func (c *Cache) publicationDefault(ctx context.Context, remote Remote, r model.Publication) error {
	if r.Ref == remote.DefaultBranch && *r.PreviousSHA != "" {
		return nil
	}
	_, err := c.policyHead(ctx, r.Repo, remote)
	var empty *gitport.EmptyRepositoryError
	if r.Ref == remote.DefaultBranch && errors.As(err, &empty) {
		return nil
	}
	return err
}

// forgeHolds reports whether the forge already has the intended object at the
// ref. A tag there at any other object is never moved.
func (c *Cache) forgeHolds(ctx context.Context, p string, remote Remote, r model.Publication) (bool, error) {
	head, err := c.remoteRef(ctx, p, remote, r.Ref)
	if err != nil && !isAbsent(err) {
		return false, err
	}
	if head.ObjectSHA == r.ObjectSHA {
		return true, nil
	}
	if strings.HasPrefix(r.Ref, "refs/tags/") && head.ObjectSHA != "" {
		return false, &gitport.AlreadyExistsError{Repository: r.Repo, Ref: r.Ref, ObjectSHA: head.ObjectSHA}
	}
	return false, nil
}

// pushIntent pushes the exact object fenced on its predecessor, then reads the
// forge back; an empty predecessor means the ref must not exist yet.
func (c *Cache) pushIntent(ctx context.Context, p string, remote Remote, r model.Publication) error {
	output, pushErr := c.pushFenced(ctx, p, remote, r)
	return c.confirmPublication(ctx, publicationAttempt{store: p, remote: remote, intent: r, output: output, err: pushErr})
}

func (c *Cache) pushFenced(ctx context.Context, p string, remote Remote, r model.Publication) ([]byte, error) {
	args := []string{"push", "--progress", "--porcelain", "--force-with-lease=" + r.Ref + ":" + *r.PreviousSHA,
		"--", remote.URL, r.ObjectSHA + ":" + r.Ref}
	return c.capture(ctx, command{dir: p, args: args, remote: &remote}, 2<<20)
}

type publicationAttempt struct {
	store  string
	remote Remote
	intent model.Publication
	output []byte
	err    error
	// gate marks the receive gate's delivery, where any rejection is final.
	gate bool
}

func (c *Cache) confirmPublication(ctx context.Context, attempt publicationAttempt) error {
	r, pushErr := attempt.intent, attempt.err
	actual, readErr := c.remoteRef(ctx, attempt.store, attempt.remote, r.Ref)
	if readErr != nil && !isAbsent(readErr) {
		return errors.Join(pushErr, readErr)
	}
	if actual.ObjectSHA == r.ObjectSHA {
		return nil
	}
	if strings.HasPrefix(r.Ref, "refs/tags/") && actual.ObjectSHA != "" {
		return &gitport.AlreadyExistsError{Repository: r.Repo, Ref: r.Ref, ObjectSHA: actual.ObjectSHA}
	}
	if actual.ObjectSHA != *r.PreviousSHA {
		return moved(r, "upstream", actual.ObjectSHA)
	}
	if finalRejection(attempt) {
		detail := rejectedPushLine(attempt.output)
		return &RefusalError{reason: reasonPublishRefused, remedy: remedyPublishRefused, Detail: detail}
	}
	return unavailable("publish confirmation", pushErr)
}
func (c *Cache) checkIntent(ctx context.Context, p string, r model.Publication, defaultBranch string) error {
	if r.ObjectSHA != "" {
		if err := c.object(ctx, p, r.ObjectSHA); err != nil {
			return err
		}
	}
	if r.PromotionID == "" {
		if err := c.checkAcceptedIntent(ctx, p, r); err != nil {
			return err
		}
	} else {
		if err := c.promotionResult(ctx, p, r.PromotionID, r.ObjectSHA); err != nil {
			return err
		}
	}
	return c.checkIntentAncestry(ctx, p, r, defaultBranch)
}
func (c *Cache) checkIntentAncestry(ctx context.Context, p string, r model.Publication, defaultBranch string) error {
	if (r.PromotionID != "" || r.Ref == defaultBranch) && r.ObjectSHA != "" && *r.PreviousSHA != "" {
		if err := c.run(ctx,
			command{dir: p,
				args: []string{"merge-base",
					"--is-ancestor",
					*r.PreviousSHA,
					r.ObjectSHA}}); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) && exit.ExitCode() == 1 {
				return refuse("publication fast-forward")
			}
			return err
		}
	}
	return nil
}

func (c *Cache) checkAcceptedIntent(ctx context.Context, p string, r model.Publication) error {
	local, err := c.resolve(ctx, p, r.Repo, r.Ref)
	if err != nil && !isAbsent(err) {
		return err
	}
	if local.ObjectSHA != r.ObjectSHA {
		return moved(r, "local", local.ObjectSHA)
	}
	if r.CommitSHA != nil && local.CommitSHA != *r.CommitSHA {
		return &RefusalError{reason: reasonPushCommit, remedy: remedyPushCommit}
	}
	return nil
}
func moved(r model.Publication, location, actual string) error {
	expected := *r.PreviousSHA
	if location == "local" {
		expected = r.ObjectSHA
	}
	return &gitport.RefMovedError{Repository: r.Repo, Ref: r.Ref, Location: location, Expected: expected, Actual: actual}
}

// Only policy rejection is permanent. Native unpack/storage failures can use
// the same rejected porcelain status and must remain retryable.
func permanentPushRefusal(output []byte) bool {
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.SplitN(line, "\t", 3)
		if len(fields) == 3 && fields[0] == "!" &&
			protectedPushRefusal(fields[2]) {
			return true
		}
	}
	return false
}

// finalRejection reports a refused push that retrying cannot change. At the
// gate any rejection is final, whatever the forge's wording: the pusher reads
// the forge's line and no retry exists. A publication keeps its retry budget
// for every rejection but a known policy refusal.
func finalRejection(a publicationAttempt) bool {
	return permanentPushRefusal(a.output) || a.gate && rejectedPushLine(a.output) != ""
}

func protectedPushRefusal(summary string) bool {
	for _, reason := range []string{"hook declined", "protected", "non-fast-forward", "deletion prohibited"} {
		if strings.Contains(summary, reason) {
			return true
		}
	}
	return false
}

func rejectedPushLine(output []byte) string {
	for _, line := range strings.Split(string(output), "\n") {
		if strings.HasPrefix(line, "!\t") {
			return line
		}
	}
	return ""
}
