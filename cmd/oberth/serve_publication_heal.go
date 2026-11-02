package main

import (
	"context"
	"errors"
	"github.com/oberthci/oberth/internal/auditanchor"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type startupPublicationStore struct{ *store.Store }

func (s startupPublicationStore) VerifyAuditState(ctx context.Context) (model.AuditHead, error) {
	return s.VerifyStartupAuditState(ctx)
}
func startupPublicationConfig(config auditanchor.ManagerConfig) auditanchor.ManagerConfig {
	if database, ok := config.Store.(*store.Store); ok {
		config.Store = startupPublicationStore{database}
	}
	return config
}
func openReconciledStartupDatabase(ctx context.Context, path string) (*store.Store, error) {
	database, err := store.OpenCurrent(ctx, path, configuredStoreOptions(ctx))
	if err != nil {
		return nil, err
	}
	if _, err = database.ReconcileInconsistentPublications(ctx); err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return database, nil
}
