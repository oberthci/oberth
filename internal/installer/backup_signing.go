package installer

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"strconv"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/stateport"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const BackupSigningSecret = "oberth-backup-signing"

// ensureBackupSigning keeps the age recipient beside the signing key: the job
// encrypts only to the recipient held at the same trust as the key that signs.
func (e *Engine) ensureBackupSigning(ctx context.Context, namespace, recipient string) (ed25519.PrivateKey, error) {
	secrets := e.Cluster.Client.CoreV1().Secrets(namespace)
	secret, err := secrets.Get(ctx, BackupSigningSecret, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		public, private, err := ed25519.GenerateKey(e.Random)
		if err != nil {
			return nil, problem("identity", nil)
		}
		defer clear(private)
		secret, err = secrets.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: BackupSigningSecret, Namespace: namespace},
			Data: map[string][]byte{"private": private, "public": public, "recipient": []byte(recipient)}}, metav1.CreateOptions{})
		if err != nil {
			return nil, problem("identity", nil)
		}
	} else if err != nil {
		return nil, problem("identity", nil)
	} else if secret.Data != nil && string(secret.Data["recipient"]) != recipient {
		secret.Data["recipient"] = []byte(recipient)
		if secret, err = secrets.Update(ctx, secret, metav1.UpdateOptions{}); err != nil {
			return nil, problem("identity", nil)
		}
	}
	key, _, err := backupSigningMaterial(secret)
	return key, err
}

// backupSigning reads the private signing key and its recipient; a nonempty
// public key must match the key.
func (e *Engine) backupSigning(ctx context.Context, namespace, public string) (ed25519.PrivateKey, string, error) {
	secret, err := e.Cluster.Client.CoreV1().Secrets(namespace).Get(ctx, BackupSigningSecret, metav1.GetOptions{})
	if err != nil {
		return nil, "", recoveryProblem()
	}
	key, recipient, err := backupSigningMaterial(secret)
	if err != nil {
		return nil, "", err
	}
	if public != "" && base64.StdEncoding.EncodeToString(key[32:]) != public {
		clear(key)
		return nil, "", recoveryProblem()
	}
	return key, recipient, nil
}

func backupSigningMaterial(secret *corev1.Secret) (ed25519.PrivateKey, string, error) {
	if secret == nil || len(secret.Data["private"]) != ed25519.PrivateKeySize || len(secret.Data["public"]) != ed25519.PublicKeySize {
		return nil, "", recoveryProblem()
	}
	defer clear(secret.Data["private"])
	key := ed25519.NewKeyFromSeed(secret.Data["private"][:ed25519.SeedSize])
	if !bytes.Equal(key, secret.Data["private"]) || !bytes.Equal(key[32:], secret.Data["public"]) {
		clear(key)
		return nil, "", recoveryProblem()
	}
	return key, string(secret.Data["recipient"]), nil
}

func backupRecipientProblem() error {
	return publicerr.New(publicerr.Forbidden, "Backup recipient does not match.", "Check who changed ConfigMap oberth-backup; run oberth upgrade to rewrite it from the profile.", nil)
}

// BackupJobConfig is public metadata; it contains no profile or private key.
type BackupJobConfig struct {
	Installation installationBackupInfo `json:"installation"`
	Options      stateport.Options      `json:"options"`
	Recipient    string                 `json:"recipient"`
	SnapshotURL  string                 `json:"snapshot_url"`
	ServicePort  int                    `json:"service_port"`
	ServerCA     []byte                 `json:"server_ca"`
}

func backupSnapshotURL(namespace string, port int) string {
	return "https://oberth." + namespace + ".svc:" + strconv.Itoa(port) + "/api/backup/database"
}

func (e *Engine) saveBackupJob(ctx context.Context, p *Plan, profile Profile) error {
	values, err := inputs(p.release.chart, p.request.Values, profile.Namespace)
	if err != nil {
		return err
	}
	port := servicePort(p.choices.HTTPSServicePort)
	config := BackupJobConfig{Recipient: profile.BackupRecipient, SnapshotURL: backupSnapshotURL(profile.Namespace, port), ServicePort: port,
		Options:  stateport.Options{Schema: p.release.schema(), Protocol: p.release.protocol()},
		ServerCA: p.identities.CA.Data["tls.crt"], Installation: installationBackupInfo{Version: 1, ServerVersion: profile.ServerVersion,
			ClusterUID: profile.ClusterUID, Namespace: profile.Namespace, PipelineNamespace: profile.PipelineNamespace,
			Values: values, BundledStore: settingText(values, "secretStore", "bundled") == "on"}}
	raw, err := json.Marshal(config)
	if err != nil {
		return recoveryProblem()
	}
	client := e.Cluster.Client.CoreV1().ConfigMaps(profile.Namespace)
	current, err := client.Get(ctx, "oberth-backup", metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = client.Create(ctx, &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "oberth-backup", Namespace: profile.Namespace}, Data: map[string]string{"configuration.json": string(raw)}}, metav1.CreateOptions{})
	} else if err == nil {
		current.Data = map[string]string{"configuration.json": string(raw)}
		_, err = client.Update(ctx, current, metav1.UpdateOptions{})
	}
	if err != nil {
		return recoveryProblem()
	}
	return nil
}
