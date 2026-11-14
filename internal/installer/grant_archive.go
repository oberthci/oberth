package installer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func inspectGrantSnapshot(ctx context.Context, path, work string, protocol store.ProtocolConfig, maxBytes int64) (summary store.ContentSummary, grants secretAccessExport, resultErr error) {
	files, err := extractDatabaseFiles(ctx, path, work, maxBytes)
	if err != nil {
		return summary, grants, err
	}
	inspection, err := store.InspectCurrent(ctx, files.Main, store.Options{Protocol: protocol, SnapshotTempDir: filepath.Dir(files.Main)})
	if err != nil {
		return summary, grants, err
	}
	defer func() { resultErr = errors.Join(resultErr, inspection.Close()) }()
	if summary, err = inspection.Summary(ctx); err != nil {
		return summary, grants, err
	}
	grants, err = exportDatabaseGrants(ctx, inspection)
	return summary, grants, err
}

func exportDatabaseGrants(ctx context.Context, db *store.Store) (secretAccessExport, error) {
	rows, err := db.SecretAccessList(ctx, "", false)
	if err != nil {
		return secretAccessExport{}, err
	}
	export := secretAccessExport{Present: true, Grants: []model.SecretAccessKey{}}
	for _, row := range rows {
		export.Grants = append(export.Grants, model.SecretAccessKey{Repo: row.Repo, Workflow: row.Workflow, Step: row.Step, Path: row.Secret})
	}
	return export, nil
}

func validateExportedGrants(keys []model.SecretAccessKey) error {
	seen := map[model.SecretAccessKey]bool{}
	for _, key := range keys {
		if err := key.Validate(); err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("duplicate archived grant key for %s workflow %s step %s", key.Repo, key.Workflow, key.Step)
		}
		seen[key] = true
	}
	return nil
}
