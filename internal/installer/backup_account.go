package installer

import (
	"context"
	"encoding/json"

	"github.com/oberthci/oberth/internal/secretstore"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

func BackupProjection(ctx context.Context, client kubernetes.Interface, namespace, audience string) ([]byte, error) {
	expires := int64(600)
	request := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{Audiences: []string{audience}, ExpirationSeconds: &expires}}
	issued, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, secretstore.BackupRole, request, metav1.CreateOptions{})
	if err != nil {
		return nil, err
	}
	if issued.Status.Token == "" {
		return nil, recoveryProblem()
	}
	return []byte(issued.Status.Token), nil
}

// ReadBackupJob reads only the installation's named public backup configuration.
func ReadBackupJob(ctx context.Context, client kubernetes.Interface, namespace string) (BackupJobConfig, error) {
	var config BackupJobConfig
	if namespace == "" {
		return config, recoveryProblem()
	}
	object, err := client.CoreV1().ConfigMaps(namespace).Get(ctx, "oberth-backup", metav1.GetOptions{})
	if err != nil || len(object.Data["configuration.json"]) > 1<<20 || json.Unmarshal([]byte(object.Data["configuration.json"]), &config) != nil {
		return config, recoveryProblem()
	}
	if config.Installation.Namespace != namespace || config.Installation.Version != 1 || config.Installation.ClusterUID == "" || config.Options.Validate() != nil || config.ServicePort < 1 || config.ServicePort > 65535 || config.SnapshotURL != backupSnapshotURL(namespace, config.ServicePort) {
		return config, recoveryProblem()
	}
	return config, nil
}

// BackupAccount uses only named Secret/config reads and a projection for itself.
// The Kubernetes Role is its authority; it never opens an administrator profile.
func (e *Engine) BackupAccount(ctx context.Context, config BackupJobConfig, c BackupConfig) (Archive, error) {
	if e.Cluster.Client == nil || c.Source == nil || c.DryRun {
		return Archive{}, recoveryProblem()
	}
	signing, recipient, err := e.backupSigning(ctx, config.Installation.Namespace, "")
	if err != nil {
		return Archive{}, err
	}
	defer clear(signing)
	if recipient == "" || recipient != config.Recipient {
		return Archive{}, backupRecipientProblem()
	}
	c.signing, c.Options, c.Recipient, c.Installation = signing, config.Options, recipient, true
	c.manifest = &bundleManifest{}
	source := installationBackupSource{database: c.Source, engine: e, info: config.Installation, manifest: c.manifest}
	if err = source.requireStore(ctx); err != nil {
		return Archive{}, err
	}
	c.Source = source
	archive, err := Backup(ctx, c)
	if archive.SHA256 != "" {
		archive.StoreSnapshotHint = source.info.storeHint()
	}
	return archive, err
}
