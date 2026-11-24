package gitcache

import (
	"context"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/model"
)

// A topic can replace the live forge head only by extending it or by replacing
// the exact head recorded at receive. Refresh never changes that evidence.
func (c *Cache) topicLease(ctx context.Context, p string, remote Remote, r model.Publication) (model.Publication, error) {
	if !ordinaryTopic(r, remote.DefaultBranch) {
		return r, nil
	}
	live, err := c.remoteRef(ctx, p, remote, r.Ref)
	if err != nil && !isAbsent(err) {
		return r, err
	}
	if r.ReceivedSHA != nil && live.ObjectSHA == *r.ReceivedSHA {
		r.PreviousSHA = new(live.ObjectSHA)
		return r, nil
	}
	if live.ObjectSHA != "" {
		if err := c.lock(ctx, p); err != nil {
			return r, err
		}
		reachable, err := c.reaches(ctx, p, live.ObjectSHA, r.ObjectSHA)
		c.unlock(p)
		if err != nil {
			return r, err
		}
		if reachable {
			r.PreviousSHA = new(live.ObjectSHA)
			return r, nil
		}
	}
	expected := ""
	if r.ReceivedSHA != nil {
		expected = *r.ReceivedSHA
	}
	return r, &gitport.RefMovedError{Repository: r.Repo, Ref: r.Ref, Location: "upstream", Expected: expected, Actual: live.ObjectSHA}
}

func ordinaryTopic(r model.Publication, defaultBranch string) bool {
	return r.PromotionID == "" && r.Ref != defaultBranch && strings.HasPrefix(r.Ref, "refs/heads/") && r.ObjectSHA != ""
}
