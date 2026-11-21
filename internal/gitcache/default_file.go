package gitcache

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// DefaultFile observes one fresh upstream default head and reads only that
// exact commit. Missing files retain the known head; outages never mean absence.
func (c *Cache) DefaultFile(ctx context.Context, repo, path string) (gitport.Ref, gitport.File, error) {
	if err := ctx.Err(); err != nil {
		return gitport.Ref{}, gitport.File{}, context.Cause(ctx)
	}
	if err := sourcecontract.ValidateFilePath(path); err != nil {
		return gitport.Ref{}, gitport.File{}, refuse("file path")
	}
	p, err := c.path(repo)
	if err != nil {
		return gitport.Ref{}, gitport.File{}, err
	}
	if err := c.refresh(ctx, repo, false); err != nil {
		return gitport.Ref{}, gitport.File{}, err
	}
	remote, err := c.remote(ctx, repo)
	if err != nil {
		return gitport.Ref{}, gitport.File{}, err
	}
	head, err := c.policyHead(ctx, p, remote)
	if err != nil {
		return head, gitport.File{}, err
	}
	file, err := c.ReadFile(ctx, gitport.Revision{Repository: repo, Commit: head.CommitSHA}, path)
	var absent *gitport.AbsentError
	if errors.As(err, &absent) && absent.Path == "" {
		err = unavailable("read fresh default commit", errors.New(absent.Error()))
	}
	return head, file, err
}
func (s *ReceiveSource) DefaultFile(ctx context.Context, repo, path string) (gitport.Ref, gitport.File, error) {
	ctx, cancel := s.scope(ctx)
	defer cancel()
	return s.base.DefaultFile(ctx, repo, path)
}
