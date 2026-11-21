package main

import (
	"context"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/stateport"
)

func (o *serverOwners) resolveRelease(ctx context.Context, repo stateport.Repository, r *app.Request, rerunOf string) error {
	if rerunOf != "" {
		old, err := o.c.store.Run(ctx, rerunOf)
		if err != nil {
			return err
		}
		r.Original, r.Release = &old, old.Release
		for _, b := range old.Admission.SourceBindings {
			if b.Descriptor.Kind == sourcecontract.Main {
				r.ReleaseHead = b.Export.DefaultBranch
				break
			}
		}
		return nil
	}
	if !service.IsReleaseTag(r.Ref.Name) {
		return nil
	}
	head, err := o.ReleaseTag(ctx, repo, r.Ref)
	r.Release, r.ReleaseHead = err == nil, head
	return err
}
