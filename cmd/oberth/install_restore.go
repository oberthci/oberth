package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

func (a application) reviewRestore(ctx context.Context, r installationRequest, out io.Writer) error {
	if !r.enabled("dry-run") {
		return installationProblem("Restore application is not available.", "Run oberth restore --from <backup> --identity-file <age-identity> --dry-run to review the archive.")
	}
	identity := r.text("identity-file")
	if identity == "" {
		session, err := a.openInstallation(r)
		if err != nil {
			return err
		}
		identity = session.profile.BackupKey
	}
	options, err := installationStateOptions(r)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("", "oberth-restore-review-*")
	if err != nil {
		return err
	}
	defer os.Remove(dir)
	inspector, err := store.NewBackup(filepath.Join(dir, "inspection.sqlite"))
	if err != nil {
		return err
	}
	review, err := installer.ReviewRestore(ctx, installer.RestoreReviewConfig{Path: r.text("from"), IdentityPath: identity, Inspector: inspector, Options: options})
	if err != nil {
		return err
	}
	if r.text("output") == "json" {
		return json.NewEncoder(out).Encode(review)
	}
	for _, line := range []string{"Backup: " + safeDisplay(review.Path), "SHA256: " + review.SHA256,
		"Source cluster: " + safeDisplay(review.ClusterUID), "Namespace: " + safeDisplay(review.Namespace),
		fmt.Sprintf("Database schema: %d; installation Secrets: %d; store snapshot: %t", review.Database.Schema, review.Secrets, review.StoreIncluded), review.Remedy} {
		if err := writeWrapped(out, line); err != nil {
			return err
		}
	}
	return nil
}
