package installer

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

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
		export.Grants = append(export.Grants, model.SecretAccessKey{Repo: row.Repo, Workflow: row.Workflow, Step: row.Step, Secret: row.Secret})
	}
	return export, nil
}

var exportedGrantPath = regexp.MustCompile(`^[A-Za-z0-9._/-]+$`)
var exportedGrantStep = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func validateExportedGrants(keys []model.SecretAccessKey) error {
	seen := map[model.SecretAccessKey]bool{}
	for _, key := range keys {
		if !exportedGrantPath.MatchString(key.Repo) || !exportedGrantPath.MatchString(key.Secret) || strings.TrimSpace(key.Step) == "" || (key.Step != "*" && (len(key.Step) > 63 || !exportedGrantStep.MatchString(key.Step))) {
			return errors.New("invalid archived grant key")
		}
		if err := model.ValidateWorkflowFile(key.Workflow); err != nil {
			return err
		}
		if seen[key] {
			return fmt.Errorf("duplicate archived grant key for %s workflow %s step %s", key.Repo, key.Workflow, key.Step)
		}
		seen[key] = true
	}
	return nil
}
