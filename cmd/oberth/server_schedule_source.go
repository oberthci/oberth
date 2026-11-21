package main

import (
	"context"
	"os"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
)

type scheduleSource struct{ cacheSource }

// A quarantined receive suspends only its repository's schedules at startup.
// A branch is looked for where a fetch would find it, on the forge too; the
// reconciliation reads that refresh itself, so it sends no ref notice.
func (s scheduleSource) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	pending, err := s.owner.cache.PendingReceive(ctx, repo)
	if err == nil && (pending.Problem != "" || pending.State == "corrupt") {
		return gitport.Ref{}, publicerr.New(publicerr.Forbidden, "Repository receive needs repair.", "Inspect the receive outbox, then retry or abandon the pending receive.", nil)
	}
	if err != nil && !os.IsNotExist(err) {
		return gitport.Ref{}, err
	}
	if err = s.owner.cache.Init(ctx, repo); err != nil {
		return gitport.Ref{}, err
	}
	return s.owner.cache.ResolveFresh(ctx, repo, ref)
}

// CachedRef resolves from the cache as it is, for the relevance check of a ref
// notice: a refresh there would take a forge move away from its notice.
func (s scheduleSource) CachedRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if err := s.owner.cache.Init(ctx, repo); err != nil {
		return gitport.Ref{}, err
	}
	return s.owner.cache.ResolveRef(ctx, repo, ref)
}
