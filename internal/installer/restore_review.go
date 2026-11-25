package installer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"reflect"
	"syscall"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
)

// RestoreReview contains only public facts from an authenticated, inspected archive.
type RestoreReview struct {
	DryRun            bool                   `json:"dry_run"`
	Path              string                 `json:"path"`
	SHA256            string                 `json:"sha256"`
	ClusterUID        string                 `json:"source_cluster_uid"`
	Namespace         string                 `json:"namespace"`
	PipelineNamespace string                 `json:"pipeline_namespace"`
	ServerVersion     string                 `json:"server_version"`
	StoreIncluded     bool                   `json:"store_included"`
	Secrets           int                    `json:"installation_secrets"`
	Database          stateport.BackupReport `json:"database"`
	Remedy            string                 `json:"remedy"`
}

const restoreReviewRemedy = "Use oberth restore --yes on a fresh cluster; keep the separate age, administrator and unseal files."

type RestoreReviewConfig struct {
	Path, IdentityPath string
	PublicKey          string
	ExpectedSHA256     string
	Inspector          stateport.Backup
	Options            stateport.Options
}

// ReviewRestore performs no cluster calls and publishes no restored files.
// Inspector owns the temporary database validation and removes its scratch files.
func ReviewRestore(ctx context.Context, config RestoreReviewConfig) (RestoreReview, error) {
	return readRestore(ctx, config, nil)
}

func readRestore(ctx context.Context, config RestoreReviewConfig, files *restoreFiles) (RestoreReview, error) {
	path, identityPath, inspector, options := config.Path, config.IdentityPath, config.Inspector, config.Options
	var result RestoreReview
	identity, err := readBackupIdentity(identityPath)
	if err != nil || inspector == nil || options.Validate() != nil {
		return result, restoreProblem()
	}
	file, err := openBackupArchive(path)
	if err != nil {
		return result, restoreProblem()
	}
	defer file.Close()
	expected, err := archiveOrigin(ctx, file, config)
	if err != nil {
		return result, err
	}
	digest := sha256.New()
	decrypted, err := age.Decrypt(io.TeeReader(archiveReader{ctx: ctx, in: file}, digest), identity...)
	if err != nil {
		return result, restoreProblem()
	}
	limit, _ := options.Limit()
	bounded := &io.LimitedReader{R: decrypted, N: limit + (64 << 10) + 1}
	result, err = inspectInstallationBundle(ctx, bundleInspection{in: bounded, inspector: inspector, options: options, files: files})
	if err == nil {
		_, err = io.Copy(io.Discard, bounded) // Authenticate the age stream through EOF.
	}
	if err != nil || bounded.N == 0 || ctx.Err() != nil || hex.EncodeToString(digest.Sum(nil)) != expected {
		return RestoreReview{}, restoreProblem()
	}
	result.Path, result.SHA256 = path, hex.EncodeToString(digest.Sum(nil))
	return result, nil
}

func readBackupIdentity(path string) ([]age.Identity, error) {
	before, err := os.Lstat(path)
	if err != nil || !private(before, false) {
		return nil, restoreProblem()
	}
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, restoreProblem()
	}
	defer file.Close()
	after, err := file.Stat()
	if err != nil || !os.SameFile(before, after) || !private(after, false) || after.Size() > 1<<20 {
		return nil, restoreProblem()
	}
	return age.ParseIdentities(file)
}

type bundleInspection struct {
	in        io.Reader
	inspector stateport.Backup
	options   stateport.Options
	files     *restoreFiles
}

func inspectInstallationBundle(ctx context.Context, c bundleInspection) (RestoreReview, error) {
	var result RestoreReview
	reader, info, err := openInstallationBundle(c.in)
	if err != nil {
		return result, restoreProblem()
	}
	if err = reader.read("database.tar", c.files.member("database.tar", func(in io.Reader) error {
		result.Database, err = c.inspector.Inspect(ctx, c.options, in)
		return err
	})); err != nil {
		return result, restoreProblem()
	}
	if err = reader.read("installation-secrets.json", func(in io.Reader) error {
		secrets, err := decodeBackupSecrets(in)
		if c.files != nil {
			c.files.secrets = secrets
		} else {
			clearBackupSecrets(secrets)
		}
		return err
	}); err != nil {
		return result, restoreProblem()
	}
	if info.StoreIncluded {
		limit, _ := c.options.Limit()
		if err = reader.read("store.snapshot", c.files.member("store.snapshot", func(in io.Reader) error { return secretstore.InspectSnapshot(in, limit) })); err != nil {
			return result, restoreProblem()
		}
	}
	expected := append([]bundleMember{}, reader.members...)
	var manifest bundleManifest
	if err = reader.json("manifest.json", &manifest, 1<<20); err != nil || !reflect.DeepEqual(manifest.Members, expected) || !reflect.DeepEqual(manifest.Report, result.Database) {
		return result, restoreProblem()
	}
	if _, err = reader.parts.NextPart(); !errors.Is(err, io.EOF) {
		return result, restoreProblem()
	}
	result.DryRun, result.ClusterUID, result.Namespace = true, info.ClusterUID, info.Namespace
	result.PipelineNamespace, result.ServerVersion = info.PipelineNamespace, info.ServerVersion
	result.StoreIncluded, result.Secrets, result.Remedy = info.StoreIncluded, len(installationSecretNames()), restoreReviewRemedy
	if c.files != nil {
		c.files.info = info
	}
	return result, nil
}

func restoreProblem() error {
	return publicerr.New(publicerr.InvalidInput, "Installation backup review failed.", "Use a complete installation backup and its owned, private age identity file; keep the originals.", nil)
}
