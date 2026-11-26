package main

import (
	"context"
	"github.com/oberthci/oberth/internal/sourcedelivery"
	"github.com/oberthci/oberth/internal/stateport"
)

func (o *serverOwners) ResumeAncestor(ctx context.Context, repo, previous, fix string) (bool, error) {
	return o.cache.ResumeAncestor(ctx, repo, previous, fix)
}

// sourceOwner follows accepted successors, then binds the tail to live labels.
// The first run remains the locator embedded in the unchanged native spec.
func (o *serverOwners) sourceOwner(ctx context.Context, id string) (stateport.Run, error) {
	r, err := o.deliveryRun(ctx, id)
	if err != nil {
		return r, err
	}
	if r.Origin.Resumed() {
		return r, &sourcedelivery.Failure{Code: "forbidden"}
	}
	seen := map[string]bool{id: true}
	for {
		next, err := o.c.store.ResumeSuccessor(ctx, r.ID)
		if err != nil {
			return r, err
		}
		if next == "" {
			return r, nil
		}
		if seen[next] {
			return r, &sourcedelivery.Failure{Code: "forbidden"}
		}
		seen[next] = true
		r, err = o.deliveryRun(ctx, next)
		if err != nil {
			return r, err
		}
	}
}
