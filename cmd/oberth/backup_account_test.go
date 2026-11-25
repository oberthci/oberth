package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	authv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestBackupAccountNeedsNoAdministratorProfileOrDecryptionKey(t *testing.T) {
	source := restoreClusterFixture(t)
	installed := installRestoreSource(t, source)
	checkRestrictedNamespace(t, source, installed.Profile.Namespace)
	configureRestoreSnapshot(t, source)
	database, options := restoreDatabaseFixture(t)
	var canonical bytes.Buffer
	expected, err := database.Snapshot(t.Context(), options, &canonical)
	if err != nil {
		t.Fatal(err)
	}
	identity := moveOperatorKeysOffProfile(t, installed.Profile)
	job, err := installer.ReadBackupJob(t.Context(), source.client, installed.Profile.Namespace)
	if err != nil || job.Options != options {
		t.Fatal("backup job configuration does not match the installation", err)
	}
	audience := job.Installation.Values["secretStore"].(map[string]any)["audience"].(string)
	limitToBackupRole(t, source, installed.Profile.Namespace, audience)
	httpClient := snapshotEndpointFixture(t, canonical.Bytes())
	engine := accountBackupEngine(source.client, nil, job)
	engine.OpenStore = source.engine.OpenStore
	archive, err := engine.BackupAccount(t.Context(), job, installer.BackupConfig{
		Source:    accountDatabase{client: source.client, config: job, http: httpClient, inspector: database},
		Directory: t.TempDir(), Keep: 1, Now: source.engine.Now()})
	if err != nil || archive.SHA256 == "" || archive.Report != expected {
		t.Fatal("backup-only account needed operator credentials", err)
	}
	review, err := installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{Path: archive.Path, IdentityPath: identity,
		PublicKey: installed.Profile.BackupPublicKey, Inspector: database, Options: options})
	if err != nil || review.Database != expected || !review.StoreIncluded || review.Secrets != 5 {
		t.Fatal("operator could not authenticate and decrypt the account's backup", err)
	}
}

func checkRestrictedNamespace(t *testing.T, source restoreCluster, name string) {
	t.Helper()
	namespace, err := source.client.CoreV1().Namespaces().Get(t.Context(), name, metav1.GetOptions{})
	if err != nil || namespace.Labels["pod-security.kubernetes.io/enforce"] != "restricted" {
		t.Fatal("backup job namespace is not Restricted", err)
	}
}

func snapshotEndpointFixture(t *testing.T, canonical []byte) *http.Client {
	t.Helper()
	return &http.Client{Transport: testTransport(func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/api/backup/database" || request.Method != http.MethodPost || request.Header.Get("Authorization") != "Bearer backup-projection-fixture" {
			t.Fatal("job did not use its projected snapshot-only credential")
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: io.NopCloser(bytes.NewReader(canonical))}, nil
	})}
}

// moveOperatorKeysOffProfile leaves the profile without its age identity or administrator key.
func moveOperatorKeysOffProfile(t *testing.T, profile installer.Profile) string {
	t.Helper()
	operator := t.TempDir()
	identity := filepath.Join(operator, "operator.age")
	if err := os.Rename(profile.BackupKey, identity); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(profile.AdminKey, filepath.Join(operator, "administrator.pem")); err != nil {
		t.Fatal(err)
	}
	return identity
}

func limitToBackupRole(t *testing.T, source restoreCluster, namespace, audience string) {
	t.Helper()
	source.client.ClearActions()
	source.client.PrependReactor("*", "*", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == namespace && (backupAccountRead(action) || backupAccountToken(action, audience)) {
			return false, nil, nil
		}
		t.Error("job attempted an operation outside its emitted Role", action.GetVerb(), action.GetResource(), action.GetSubresource())
		return true, nil, errors.New("backup account cannot perform this operation")
	})
}

func backupAccountRead(action ktesting.Action) bool {
	get, ok := action.(ktesting.GetAction)
	if !ok || action.GetVerb() != "get" {
		return false
	}
	switch action.GetResource().Resource {
	case "configmaps":
		return get.GetName() == "oberth-backup"
	case "secrets":
		return slices.Contains([]string{"oberth-admin", "oberth-ca", "oberth-ssh-host", "oberth-forge-key", "oberth-store-tls", installer.BackupSigningSecret}, get.GetName())
	}
	return false
}

func backupAccountToken(action ktesting.Action, storeAudience string) bool {
	create, ok := action.(ktesting.CreateActionImpl)
	if !ok || action.GetVerb() != "create" || action.GetResource().Resource != "serviceaccounts" || action.GetSubresource() != "token" || create.Name != secretstore.BackupRole {
		return false
	}
	request, ok := create.GetObject().(*authv1.TokenRequest)
	return ok && request.Spec.ExpirationSeconds != nil && *request.Spec.ExpirationSeconds == 600 && len(request.Spec.Audiences) == 1 && slices.Contains([]string{apicontract.BackupSnapshotAudience, storeAudience}, request.Spec.Audiences[0])
}

type accountBackupFixture struct {
	source  restoreCluster
	job     installer.BackupJobConfig
	profile installer.Profile
	backup  installer.BackupConfig
	engine  *installer.Engine
}

func newAccountBackupFixture(t *testing.T) accountBackupFixture {
	t.Helper()
	source := restoreClusterFixture(t)
	installed := installRestoreSource(t, source)
	configureRestoreSnapshot(t, source)
	database, options := restoreDatabaseFixture(t)
	var canonical bytes.Buffer
	if _, err := database.Snapshot(t.Context(), options, &canonical); err != nil {
		t.Fatal(err)
	}
	job, err := installer.ReadBackupJob(t.Context(), source.client, installed.Profile.Namespace)
	if err != nil {
		t.Fatal(err)
	}
	engine := accountBackupEngine(source.client, nil, job)
	engine.OpenStore = source.engine.OpenStore
	dir := t.TempDir()
	if err = os.WriteFile(filepath.Join(dir, "oberth-backup-20260101T000000.000000000Z.age"), []byte("prior-restorable-archive"), 0600); err != nil {
		t.Fatal(err)
	}
	return accountBackupFixture{source: source, job: job, profile: installed.Profile, engine: engine, backup: installer.BackupConfig{
		Source:    accountDatabase{client: source.client, config: job, http: snapshotEndpointFixture(t, canonical.Bytes()), inspector: database},
		Directory: dir, Keep: 1, Now: source.engine.Now()}}
}

func (f accountBackupFixture) unchanged(t *testing.T) {
	t.Helper()
	entries, err := os.ReadDir(f.backup.Directory)
	if err != nil || len(entries) != 1 || entries[0].Name() != "oberth-backup-20260101T000000.000000000Z.age" {
		t.Fatal("refused account backup published an archive or pruned a restorable one", err, len(entries))
	}
}

func TestBackupAccountWithRefusedStoreRoleFailsAndKeepsArchives(t *testing.T) {
	f := newAccountBackupFixture(t)
	f.source.client.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		request, ok := action.(ktesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if action.GetSubresource() != "token" || !ok || request.Spec.Audiences[0] == apicontract.BackupSnapshotAudience {
			return false, nil, nil
		}
		return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "serviceaccounts"}, secretstore.BackupRole, errors.New("store role refused"))
	})
	_, err := f.engine.BackupAccount(t.Context(), f.job, f.backup)
	if err == nil || !strings.Contains(err.Error(), "Store snapshot access is missing") {
		t.Fatal("account backup without the store snapshot counted as good", err)
	}
	f.unchanged(t)
}

func TestBackupAccountRefusesRecipientOutsideSigningSecret(t *testing.T) {
	f := newAccountBackupFixture(t)
	signing, err := f.source.client.CoreV1().Secrets(f.profile.Namespace).Get(t.Context(), installer.BackupSigningSecret, metav1.GetOptions{})
	if err != nil || string(signing.Data["recipient"]) != f.profile.BackupRecipient {
		t.Fatal("signing Secret does not hold the profile's backup recipient", err)
	}
	attacker, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	configMaps := f.source.client.CoreV1().ConfigMaps(f.profile.Namespace)
	edited, err := configMaps.Get(t.Context(), "oberth-backup", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	f.job.Recipient = attacker.Recipient().String()
	raw, err := json.Marshal(f.job)
	if err != nil {
		t.Fatal(err)
	}
	edited.Data["configuration.json"] = string(raw)
	if _, err = configMaps.Update(t.Context(), edited, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	if f.job, err = installer.ReadBackupJob(t.Context(), f.source.client, f.profile.Namespace); err != nil || f.job.Recipient != attacker.Recipient().String() {
		t.Fatal("edited ConfigMap was not read", err)
	}
	if _, err = f.engine.BackupAccount(t.Context(), f.job, f.backup); err == nil || !strings.Contains(err.Error(), "Backup recipient does not match") {
		t.Fatal("account backup encrypted to an edited ConfigMap recipient", err)
	}
	f.unchanged(t)
}

// The in-cluster job prunes only signed full archives and leaves database-only copies alone.
func TestBackupAccountRetentionPrunesOnlyFullArchives(t *testing.T) {
	f := newAccountBackupFixture(t)
	full := "oberth-backup-20260101T000000.000000000Z.age"
	partial := "oberth-backup-20260102T000000.000000000Z.age"
	for _, name := range []string{full + ".manifest.json", partial} {
		if err := os.WriteFile(filepath.Join(f.backup.Directory, name), []byte("prior-archive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	archive, err := f.engine.BackupAccount(t.Context(), f.job, f.backup)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(f.backup.Directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{partial, filepath.Base(archive.Path), filepath.Base(archive.Path) + ".manifest.json"}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatal("account retention pruned a database-only copy or kept an expired full archive", names)
	}
}
