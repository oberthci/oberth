package installer

import (
	"context"
	"encoding/json"
	"io"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func installationSecretNames() []string {
	return []string{"oberth-admin", "oberth-ca", "oberth-ssh-host", "oberth-forge-key", "oberth-store-tls"}
}

type backupSecret struct {
	Name string            `json:"name"`
	Type corev1.SecretType `json:"type"`
	Data map[string][]byte `json:"data"`
}

type installationBackupInfo struct {
	Version           int            `json:"version"`
	ServerVersion     string         `json:"server_version"`
	ClusterUID        string         `json:"cluster_uid"`
	Namespace         string         `json:"namespace"`
	PipelineNamespace string         `json:"pipeline_namespace"`
	BundledStore      bool           `json:"bundled_store"`
	StoreIncluded     bool           `json:"store_included"`
	Values            map[string]any `json:"values"`
}

// BackupInstallation binds all private reads to a fresh owner and cluster check.
// Database-only upgrade archives continue to use Backup directly.
func (e *Engine) BackupInstallation(ctx context.Context, c BackupConfig) (Archive, error) {
	if c.DryRun {
		return Backup(ctx, c)
	}
	if c.Source == nil {
		return Archive{}, problem("wiring", nil)
	}
	unlock, err := e.Credentials.lock()
	if err != nil {
		return Archive{}, err
	}
	defer unlock()
	profile, err := e.Credentials.profile()
	if err != nil {
		return Archive{}, problem("custody", nil)
	}
	uid, err := e.Cluster.UID(ctx)
	if err != nil {
		return Archive{}, problem("cluster", nil)
	}
	if err = e.Credentials.checkTarget(e.Cluster.Context, uid, profile.Namespace); err != nil {
		return Archive{}, err
	}
	if c.Now.IsZero() && e.Now != nil {
		c.Now = e.Now()
	}
	source, err := e.installationBackupSource(ctx, c.Source, profile, uid)
	if err != nil {
		return Archive{}, err
	}
	c.signing, _, err = e.backupSigning(ctx, profile.Namespace, profile.BackupPublicKey)
	if err != nil {
		return Archive{}, err
	}
	defer clear(c.signing)
	c.manifest = &bundleManifest{}
	source.manifest = c.manifest
	c.Source, c.Installation = source, true
	archive, err := Backup(ctx, c)
	if archive.SHA256 != "" {
		archive.StoreSnapshotHint = source.info.storeHint()
	}
	return archive, err
}

func (e *Engine) installationBackupSource(ctx context.Context, database SnapshotPort, profile Profile, uid string) (installationBackupSource, error) {
	history, err := e.ownedHistory(ctx, profile.Namespace)
	if err != nil {
		return installationBackupSource{}, err
	}
	values := history.Deployed.Values
	if history.Deployed.Number == 0 || len(values) == 0 {
		return installationBackupSource{}, recoveryProblem()
	}
	source := installationBackupSource{database: database, engine: e, info: installationBackupInfo{
		Version: 1, ServerVersion: profile.ServerVersion, ClusterUID: uid, Namespace: profile.Namespace,
		PipelineNamespace: profile.PipelineNamespace, Values: values,
		BundledStore: settingText(values, "secretStore", "bundled") == "on",
	}}
	err = source.requireStore(ctx)
	return source, err
}

// requireStore fails a bundled-store backup that cannot include the snapshot:
// restore refuses such an archive, so it must never publish or drive retention.
func (s *installationBackupSource) requireStore(ctx context.Context) error {
	s.info.StoreIncluded = false
	if !s.info.BundledStore {
		return nil
	}
	available, err := s.snapshotAvailable(ctx)
	if err != nil {
		return err
	}
	if !available {
		return storeAccessProblem()
	}
	s.info.StoreIncluded = true
	return nil
}

func (i installationBackupInfo) storeHint() string {
	if i.StoreIncluded {
		return "Includes installation Secrets and the bundled store snapshot."
	}
	return "Includes installation Secrets; external-store backups remain operator-owned."
}

func storeAccessProblem() error {
	return publicerr.New(publicerr.Forbidden, "Store snapshot access is missing.", "Run oberth secretstore setup --backup-only, then back up again; nothing was written or pruned.", nil)
}

type installationBackupSource struct {
	database SnapshotPort
	engine   *Engine
	info     installationBackupInfo
	manifest *bundleManifest
}

func (s installationBackupSource) Snapshot(ctx context.Context, options stateport.Options, out io.Writer) (stateport.BackupReport, error) {
	private, err := s.engine.backupSecrets(ctx, s.info.Namespace)
	if err != nil {
		return stateport.BackupReport{}, err
	}
	defer clear(private)
	return (installationBundle{info: s.info, secrets: private, database: s.database, store: s.storeSnapshot, manifest: s.manifest}).write(ctx, out, options)
}

func (e *Engine) backupSecrets(ctx context.Context, namespace string) ([]byte, error) {
	var saved []backupSecret
	defer func() {
		for _, secret := range saved {
			for _, value := range secret.Data {
				clear(value)
			}
		}
	}()
	for _, name := range installationSecretNames() {
		secret, err := e.Cluster.Client.CoreV1().Secrets(namespace).Get(ctx, name, metav1.GetOptions{})
		valid := validSecret(secret)
		if secret != nil && name == "oberth-admin" {
			valid = len(secret.Data["admin.pub"]) > 0 && len(secret.Data["push.pub"]) > 0
		}
		if err != nil || !valid {
			return nil, recoveryProblem()
		}
		saved = append(saved, backupSecret{Name: name, Type: secret.Type, Data: secret.DeepCopy().Data})
	}
	return json.Marshal(saved)
}

func (s installationBackupSource) storeSnapshot(ctx context.Context, out io.Writer, limit int64) error {
	available, err := s.withBackupStore(ctx, func(client *secretstore.Client, request secretstore.SnapshotRequest) error {
		request.Limit = limit
		return client.Snapshot(ctx, request, out)
	})
	if !available && err == nil {
		return storeAccessProblem()
	}
	return err
}

func (s installationBackupSource) snapshotAvailable(ctx context.Context) (bool, error) {
	var available bool
	_, err := s.withBackupStore(ctx, func(client *secretstore.Client, request secretstore.SnapshotRequest) (err error) {
		available, err = client.SnapshotAvailable(ctx, request)
		return err
	})
	return available, err
}

func (s installationBackupSource) withBackupStore(ctx context.Context, call func(*secretstore.Client, secretstore.SnapshotRequest) error) (bool, error) {
	cfg := secretstore.Config{ServerNamespace: s.info.Namespace,
		AuthMount: valueText(s.info.Values, "secretStore", "auth_mount"),
		Audience:  valueText(s.info.Values, "secretStore", "audience"),
	}
	jwt, err := BackupProjection(ctx, s.engine.Cluster.Client, cfg.ServerNamespace, cfg.Audience)
	if apierrors.IsNotFound(err) || apierrors.IsForbidden(err) {
		return false, nil
	}
	if err != nil {
		return false, recoveryProblem()
	}
	defer clear(jwt)
	client, closeClient, err := s.engine.storeClient(ctx, cfg)
	if err != nil {
		return false, recoveryProblem()
	}
	defer closeClient()
	defer client.Close()
	if err = call(client, secretstore.SnapshotRequest{Config: cfg, JWT: jwt}); err != nil {
		return false, recoveryProblem()
	}
	return true, nil
}

func valueText(values map[string]any, section, field string) string {
	block, _ := values[section].(map[string]any)
	text, _ := block[field].(string)
	return text
}
