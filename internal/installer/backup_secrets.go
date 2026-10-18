package installer

// `oberth backup export-secrets` and `export-values`: an archive's OpenBao KV
// secrets as one plain JSON document that any KV v2 store (OpenBao, Vault)
// can import with its own CLI, independent of Oberth and of the store's raft
// state; and the release's user-supplied Helm values as a values file.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"sigs.k8s.io/yaml"
)

// secretsExportFormat names the document written by ExportSecrets.
const secretsExportFormat = "oberth.secrets/v1" //nolint:gosec // G101: a format name, not a credential

// SecretsExport is the universal secrets document: KV v2 paths under Mount
// with their current key/value data.
type SecretsExport struct {
	Format     string                 `json:"format"`
	ExportedAt string                 `json:"exported_at"`
	Source     string                 `json:"source"`
	KVVersion  int                    `json:"kv_version"`
	Mount      string                 `json:"mount"`
	Secrets    []SecretsExportElement `json:"secrets"`
}

// SecretsExportElement is one secret.
type SecretsExportElement struct {
	Path string         `json:"path"`
	Data map[string]any `json:"data"`
}

// ExportSecretsConfig selects the archive and its key.
type ExportSecretsConfig struct {
	Input      string
	PrivateKey string
}

// ExportSecrets decrypts and verifies an archive and writes its secrets to
// out as a SecretsExport. It needs no cluster.
func ExportSecrets(ctx context.Context, cfg ExportSecretsConfig, host InstallDeps, out io.Writer) error {
	return exportFromArchive(ctx, cfg, host, func(archive restoreArchive) error { return writeSecretsExport(out, archive) })
}

// ExportValues writes the release's user-supplied Helm values an archive
// recorded, as YAML. Values the installer sets itself are included too: they
// describe the source install, so review them before passing the file to
// oberth upgrade -f.
func ExportValues(ctx context.Context, cfg ExportSecretsConfig, host InstallDeps, out io.Writer) error {
	return exportFromArchive(ctx, cfg, host, func(archive restoreArchive) error { return writeValuesExport(out, archive) })
}

func writeValuesExport(out io.Writer, archive restoreArchive) error {
	if archive.Manifest.Scope == BackupScopeHistory {
		return errors.New("this archive holds history only (oberth backup --history-only): it has no Helm values")
	}
	body, err := yaml.Marshal(archive.Values)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "# Helm values of release oberth recorded by oberth backup on %s (context %s).\n"+
		"# image, secretstore, tls.pki, certificate pins and other installer-set values describe the SOURCE install:\n"+
		"# keep only your own settings before using this with oberth upgrade -f.\n%s",
		archive.Manifest.CreatedAt, archive.Manifest.Source.Context, body)
	return err
}

func exportFromArchive(ctx context.Context, cfg ExportSecretsConfig, host InstallDeps, write func(restoreArchive) error) (resultErr error) {
	if err := validateBackupPrivateKey(cfg.PrivateKey); err != nil {
		return err
	}
	host = host.withDefaults()
	openssl, err := host.LookPath("openssl")
	if err != nil {
		return errors.New("openssl not found in PATH; backup archives use OpenSSL 3 `cms` (AES-256-GCM), install it and rerun")
	}
	deps := backupDeps{Deps: Deps{Output: io.Discard}, openssl: openssl, now: time.Now,
		cipher: opensslCMS{binary: openssl, privateKey: cfg.PrivateKey}}
	work, err := os.MkdirTemp("", "oberth-restore-")
	if err != nil {
		return fmt.Errorf("create private working directory: %w", err)
	}
	defer func() { resultErr = errors.Join(resultErr, os.RemoveAll(work)) }()
	archive, err := loadRestoreArchive(ctx, RestoreConfig{Input: cfg.Input}, deps, work)
	if err != nil {
		return err
	}
	return write(archive)
}

func writeSecretsExport(out io.Writer, archive restoreArchive) error {
	if archive.Manifest.Scope == BackupScopeHistory {
		return errors.New("this archive holds history only (oberth backup --history-only): it has no secrets")
	}
	doc := SecretsExport{
		Format: secretsExportFormat, ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Source: archive.Manifest.CreatedAt + " " + archive.Manifest.Source.Context, KVVersion: 2, Mount: archive.KV.Mount,
		Secrets: make([]SecretsExportElement, 0, len(archive.KV.Secrets)),
	}
	for _, secret := range archive.KV.Secrets {
		doc.Secrets = append(doc.Secrets, SecretsExportElement(secret))
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(doc)
}
