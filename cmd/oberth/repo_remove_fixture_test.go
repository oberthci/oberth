package main

import (
	"context"
	"path/filepath"

	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func removeTestRepository(ctx context.Context, path, root, input string) (store.RemovedRepository, error) {
	if root == "/data/git" {
		root = filepath.Join(filepath.Dir(path), "git")
	}
	database, err := storetest.OpenAdminClient(ctx, path, store.Options{})
	if err != nil {
		return store.RemovedRepository{}, err
	}
	defer database.Close()
	upstreams := app.Upstreams{Catalog: database}
	cache, err := gitcache.New(gitcache.Config{Root: root, Upstream: upstreams.Remote, RepoQualifier: upstreams.QualifyInput})
	if err != nil {
		return store.RemovedRepository{}, err
	}
	return (lockedRepositoryRemover{database: database, cache: cache}).RemoveRepository(ctx, administrativeActor, input)
}
