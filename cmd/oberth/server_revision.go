package main

import (
	"context"
	"errors"
	"strings"

	"github.com/oberthci/oberth/internal/gitoid"
	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func (o *serverOwners) resolveInput(ctx context.Context, repo, selector string) (gitport.Ref, error) {
	if !gitoid.Valid(selector) {
		return o.ResolveRef(ctx, repo, selector)
	}
	refs, err := o.ListRefs(ctx, repo)
	if err != nil {
		return gitport.Ref{}, err
	}
	for _, ref := range refs {
		if ref.ObjectSHA == selector {
			return ref, nil
		}
	}
	_, reachable, err := o.cache.Pin(ctx, sourcecontract.Revision{Repository: repo, Commit: selector})
	if err != nil {
		return gitport.Ref{}, err
	}
	if reachable {
		return gitport.Ref{Name: o.settings.DefaultBranch, ObjectSHA: selector, CommitSHA: selector}, nil
	}
	rows, err := o.c.store.Runs(ctx, stateport.RunFilter{Repositories: []string{repo}, CommitSHA: selector, Limit: 1})
	if err != nil {
		return gitport.Ref{}, err
	}
	if len(rows) == 1 {
		return gitport.Ref{Name: rows[0].Ref, ObjectSHA: selector, CommitSHA: selector}, nil
	}
	return gitport.Ref{}, publicerr.New(publicerr.NotFound, "The revision has no published reference or recorded run.", "Push the revision to a branch before checking it.", nil)
}

func (o *serverOwners) ResolveRef(ctx context.Context, repo, ref string) (gitport.Ref, error) {
	if strings.HasPrefix(ref, "refs/") {
		return (cacheSource{o}).ResolveRef(ctx, repo, ref)
	}
	result, err := (cacheSource{o}).ResolveRef(ctx, repo, "refs/heads/"+ref)
	var absent *gitport.AbsentError
	if !errors.As(err, &absent) {
		return result, err
	}
	return (cacheSource{o}).ResolveRef(ctx, repo, "refs/tags/"+ref)
}
