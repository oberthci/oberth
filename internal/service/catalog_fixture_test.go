package service_test

import (
	"context"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func (f *fixtureStore) RepositoryScope(ctx context.Context, scope []string) ([]string, error) {
	if err := f.native.Before("RepositoryScope", false); err != nil {
		return nil, err
	}
	return f.native.RepositoryScope(ctx, scope)
}
func (f *fixtureStore) Repository(ctx context.Context, name string, scope []string) (stateport.Repository, error) {
	if err := f.native.Before("Repository", false); err != nil {
		return stateport.Repository{}, err
	}
	return f.native.Repository(ctx, name, scope)
}
func (f *fixtureStore) Repositories(ctx context.Context, q stateport.CatalogFilter) ([]stateport.Repository, error) {
	if err := f.native.Before("Repositories", false); err != nil {
		return nil, err
	}
	return f.native.Repositories(ctx, q)
}
func (f *fixtureStore) Steps(ctx context.Context, id string) ([]stepmodel.RunStep, error) {
	if err := f.native.Before("Steps", false); err != nil {
		return nil, err
	}
	return f.native.Steps(ctx, id)
}
