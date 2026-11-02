package app

import (
	"context"
	"errors"
	"testing"

	"github.com/oberthci/oberth/internal/model"
)

type unavailableScopeCatalog struct{ UpstreamCatalog }

func (unavailableScopeCatalog) RepositoryByName(context.Context, string) (model.Repository, error) {
	return model.Repository{}, context.DeadlineExceeded
}
func (unavailableScopeCatalog) ListUpstreams(context.Context) ([]model.Upstream, error) {
	return nil, context.DeadlineExceeded
}
func TestQualifyInScopePreservesDatabaseFailure(t *testing.T) {
	r := Upstreams{Catalog: unavailableScopeCatalog{}}
	_, err := r.QualifyInScope("oberth", []string{"origin"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("DB failure became scope refusal: %v", err)
	}
}
