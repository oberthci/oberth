package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/client"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/store"
)

func (a application) reviewRestore(ctx context.Context, r installationRequest, out io.Writer) error {
	verifyOnly := r.enabled("dry-run") || r.name == "backup verify"
	if !verifyOnly && !r.enabled("yes") {
		return installationProblem("Restore needs an explicit choice.", "Use --dry-run for offline review, or --yes with a fresh cluster and profile to restore.")
	}
	identity, err := a.restoreIdentity(r)
	if err != nil {
		return err
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
	public, err := a.backupPublicKey(r)
	if err != nil {
		return err
	}
	config := installer.RestoreReviewConfig{Path: r.text("from"), IdentityPath: identity, Inspector: inspector, Options: options,
		PublicKey: public, ExpectedSHA256: r.text("sha256")}
	review, err := installer.ReviewRestore(ctx, config)
	if err != nil {
		return err
	}
	if !verifyOnly {
		return a.applyRestore(ctx, r, config, out)
	}
	return printRestoreReview(out, r, review)
}

func printRestoreReview(out io.Writer, r installationRequest, review installer.RestoreReview) error {
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

func (a application) restoreIdentity(r installationRequest) (string, error) {
	if identity := r.text("identity-file"); identity != "" {
		return identity, nil
	}
	profile, err := a.backupOperatorProfile(r)
	return profile.BackupKey, err
}

func (a application) backupOperatorProfile(r installationRequest) (installer.Profile, error) {
	name := r.text("backup-profile")
	if name == "" {
		name = installationProfileName(r)
	}
	if name != "" {
		dir, err := client.ProfileDir(a.base, name)
		if err != nil {
			return installer.Profile{}, err
		}
		return readInstallationProfile(dir, false)
	}
	_, dir, err := client.SelectedProfile(a.base, name)
	if err != nil {
		return installer.Profile{}, err
	}
	return readInstallationProfile(dir, false)
}

func (a application) backupPublicKey(r installationRequest) (string, error) {
	if r.text("sha256") != "" {
		return "", nil
	}
	profile, err := a.backupOperatorProfile(r)
	return profile.BackupPublicKey, err
}

func restoreInputs(r installationRequest) ([]byte, []byte, error) {
	if r.text("context") == "" || r.text("url") == "" || r.text("profile") == "" || r.text("admin-key-file") == "" {
		return nil, nil, installationProblem("Restore target or recovery files are missing.", "Pass --context, --url, a new --profile and --admin-key-file; bundled stores also need --unseal-file.")
	}
	administrator, err := client.ReadPrivate(r.text("admin-key-file"))
	if err != nil {
		return nil, nil, err
	}
	if r.text("unseal-file") == "" {
		return administrator, nil, nil
	}
	unseal, err := client.ReadPrivate(r.text("unseal-file"))
	if err != nil {
		clear(administrator)
		return nil, nil, err
	}
	return administrator, unseal, nil
}

func (a application) applyRestore(ctx context.Context, r installationRequest, config installer.RestoreReviewConfig, out io.Writer) error {
	administrator, unseal, err := restoreInputs(r)
	if err != nil {
		return err
	}
	defer clear(administrator)
	defer clear(unseal)
	release, err := installationRelease(ctx, r)
	if err != nil {
		return err
	}
	s, err := a.openInstallation(r)
	if err != nil {
		return err
	}
	if err = s.openEngine(); err != nil {
		return err
	}
	defer s.engine.Credentials.Close()
	result, err := s.engine.Restore(ctx, installer.RestoreConfig{Archive: config, Release: release, Choices: s.choices, Administrator: administrator, Unseal: bytes.TrimSpace(unseal), Yes: true})
	if err != nil {
		return err
	}
	if r.text("output") == "json" {
		return json.NewEncoder(out).Encode(result)
	}
	return showInstallation(out, result)
}
