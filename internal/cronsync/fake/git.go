package fake

import (
	"context"
	"sync"

	"github.com/oberthci/oberth/internal/gitport"
)

type Git struct {
	mu    sync.Mutex
	hook  Hook
	refs  map[string]gitport.Ref
	files map[gitport.Revision]map[string]gitport.File
}

func NewGit(hook Hook) *Git {
	return &Git{hook: hook, refs: map[string]gitport.Ref{}, files: map[gitport.Revision]map[string]gitport.File{}}
}
func (g *Git) PutRef(repo string, ref gitport.Ref) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refs[repo+"#"+ref.Name] = ref
	revision := gitport.Revision{Repository: repo, Commit: ref.CommitSHA}
	if g.files[revision] == nil {
		g.files[revision] = map[string]gitport.File{}
	}
}
func (g *Git) PutFile(revision gitport.Revision, file gitport.File) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.files[revision] == nil {
		g.files[revision] = map[string]gitport.File{}
	}
	g.files[revision][file.Path] = file.Clone()
}
func (g *Git) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if err := g.before(ctx, "resolve", ref); err != nil {
		return gitport.Ref{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if value, found := g.refs[repo+"#"+ref]; found {
		return value, nil
	}
	return gitport.Ref{}, &gitport.AbsentError{Repository: repo, Ref: ref}
}
func (g *Git) ReadFile(ctx context.Context, revision gitport.Revision, path string) (gitport.File, error) {
	if err := g.before(ctx, "read", path); err != nil {
		return gitport.File{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	files, exists := g.files[revision]
	if !exists {
		return gitport.File{}, &gitport.AbsentError{Repository: revision.Repository, Ref: revision.Commit}
	}
	file, exists := files[path]
	if !exists {
		return gitport.File{}, &gitport.AbsentError{Repository: revision.Repository, Ref: revision.Commit, Path: path}
	}
	if err := file.Validate(revision); err != nil {
		return gitport.File{}, err
	}
	return file.Clone(), nil
}
func (g *Git) before(ctx context.Context, verb, path string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if g.hook != nil {
		if err := g.hook(ctx, verb, path); err != nil {
			return err
		}
	}
	return ctx.Err()
}
