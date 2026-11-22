package app

import (
	"context"
	"errors"
	"time"

	"github.com/oberthci/oberth/internal/gitport"
)

type ProbeKey struct{ Address, Fingerprint, PinsDigest, Repository string }
type ProbeResult struct {
	Reason, Detail string
}

// Checks uses Git errors; composition binds ProbeKey to the actual transport identity.
type Checks struct {
	Git      gitport.Git
	Deadline Deadline
}

func (c *Checks) CheckRepository(ctx context.Context, key ProbeKey) ProbeResult {
	result := ProbeResult{Reason: messages()["ready"].reason}
	ctx, cancel := c.Deadline(ctx, 15*time.Second)
	defer cancel()
	_, err := c.Git.RemoteRef(ctx, key.Repository, "HEAD")
	if err != nil {
		result.Reason = messages()["probe"].reason
		result.Detail = err.Error()
		var empty *gitport.EmptyRepositoryError
		if errors.As(err, &empty) {
			result.Reason = empty.Reason()
		}
		var unfinished *gitport.CloneUnfinishedError
		if errors.As(err, &unfinished) {
			result.Reason = unfinished.Reason()
		}
		var updating *gitport.FetchUnfinishedError
		if errors.As(err, &updating) {
			result.Reason = updating.Reason()
		}
		var outage *gitport.UnavailableError
		if errors.As(err, &outage) && outage.Op == "forge" {
			result.Reason = outage.Reason()
			result.Detail = outage.Cause.Error()
		}
	}
	return result
}
