package service

import (
	"context"

	"github.com/oberthci/oberth/internal/actor"
	"github.com/oberthci/oberth/internal/apicontract"
)

// scopedRepoUpstreams exposes only public addresses in the caller's live scope.
func (s *Service) scopedRepoUpstreams(ctx context.Context, a actor.Actor) ([]apicontract.Upstream, error) {
	if s.c.People.Admin == nil {
		return []apicontract.Upstream{}, nil
	}
	bindings, err := s.c.People.Admin.UpstreamBindings(ctx)
	if err != nil {
		return nil, err
	}
	out := []apicontract.Upstream{}
	for _, u := range bindings {
		if a.CanTouchRepository(u.Name + "/" + u.Org + "/repository") {
			out = append(out, apicontract.Upstream{Name: u.Name, Org: u.Org, BaseURL: u.BaseURL})
		}
	}
	return out, nil
}
