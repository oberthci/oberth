package installer_test

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	storefake "github.com/oberthci/oberth/internal/secretstore/fake"
	authv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"
)

type snapshotHTTP func(*http.Request) (*http.Response, error)

func (f snapshotHTTP) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInstallationBackupDryRunIsLocal(t *testing.T) {
	f := setup(t)
	f.client.ClearActions()
	database, options := backupFixture(t)
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "planned.age")
	archive, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{
		Source: database, Options: options, Recipient: identity.Recipient().String(), Path: path, DryRun: true,
	})
	if err != nil || !archive.DryRun || archive.Path != path || len(f.client.Actions()) != 0 {
		t.Fatal("dry run requires cluster or profile", archive, err)
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("dry run published an archive", err)
	}
}

func TestInstallationBackupIncludesSecretsAndStoreAndReviewsOffline(t *testing.T) {
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	installSnapshotFixture(t, f)
	database, options := backupFixture(t)
	path := filepath.Join(t.TempDir(), "complete.age")
	archive, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Path: path})
	if err != nil {
		t.Fatal(err)
	}
	assertPrivateBackupContents(t, path, installed.Profile.BackupKey)
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal("installation backup is not private", err)
	}
	// The review gets no cluster client and must work after cluster access is gone.
	f.client.ClearActions()
	review, err := installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{Path: path, IdentityPath: installed.Profile.BackupKey, PublicKey: installed.Profile.BackupPublicKey, Inspector: database, Options: options})
	if err != nil {
		t.Fatal(err)
	}
	assertOfflineBackupReview(t, f, review, archive)
	corrupt := changedSnapshotArchive(t, path, installed.Profile.BackupKey, installed.Profile.BackupRecipient)
	if _, err = installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{Path: corrupt, IdentityPath: installed.Profile.BackupKey, ExpectedSHA256: trustedTestArchiveDigest(t, corrupt), Inspector: database, Options: options}); err == nil {
		t.Fatal("store snapshot with a mismatched member digest passed review")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err = os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{Path: path, IdentityPath: installed.Profile.BackupKey, ExpectedSHA256: trustedTestArchiveDigest(t, path), Inspector: database, Options: options}); err == nil {
		t.Fatal("tampered final age chunk passed review")
	}
}

// Corruption tests explicitly trust their fixture digest so origin rejection
// cannot mask the independent member and age-authentication guards.
func trustedTestArchiveDigest(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func assertOfflineBackupReview(t *testing.T, f fixture, review installer.RestoreReview, archive installer.Archive) {
	t.Helper()
	if !review.DryRun || review.Secrets != 5 || !review.StoreIncluded || review.Database != archive.Report || review.Namespace != "oberth" || review.SHA256 != archive.SHA256 {
		t.Fatal("complete backup did not pass offline review", review)
	}
	if len(f.client.Actions()) != 0 || strings.Contains(jsonOf(t, review), "snapshot-private-fixture") {
		t.Fatal("review contacted a cluster or exposed private bytes")
	}
}

func changedSnapshotArchive(t *testing.T, path, identityPath, recipientText string) string {
	t.Helper()
	identity, err := os.Open(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	identities, err := age.ParseIdentities(identity)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(raw), identities...)
	if err != nil {
		t.Fatal(err)
	}
	contents, err := io.ReadAll(plain)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(contents)
	changedSnapshot, err := storefake.Snapshot([]byte("snapshot-changed-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	contents = bytes.ReplaceAll(contents, nativeSnapshotFixture(t), changedSnapshot)
	recipient, err := age.ParseX25519Recipient(recipientText)
	if err != nil {
		t.Fatal(err)
	}
	var encrypted bytes.Buffer
	w, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Write(contents); err != nil {
		t.Fatal(err)
	}
	if err = w.Close(); err != nil {
		t.Fatal(err)
	}
	changed := filepath.Join(t.TempDir(), "changed.age")
	if err = os.WriteFile(changed, encrypted.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return changed
}

func installSnapshotFixture(t *testing.T, f fixture) {
	t.Helper()
	f.client.PrependReactor("create", "serviceaccounts", func(action ktesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		request := action.(ktesting.CreateAction).GetObject().(*authv1.TokenRequest)
		if action.GetNamespace() != "oberth" || len(request.Spec.Audiences) != 1 || request.Spec.Audiences[0] != "oberth-store" || *request.Spec.ExpirationSeconds != 600 {
			t.Fatal("backup requested broader cluster authority")
		}
		return true, &authv1.TokenRequest{Status: authv1.TokenRequestStatus{Token: "backup-projection-fixture"}}, nil
	})
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: snapshotHTTP(func(r *http.Request) (*http.Response, error) {
			return installationSnapshotResponse(t, r)
		})})
		return client, func() {}, err
	}
}

func installationSnapshotResponse(t *testing.T, r *http.Request) (*http.Response, error) {
	t.Helper()
	body := ""
	switch r.URL.Path {
	case "/v1/auth/kubernetes/login":
		var args map[string]string
		if json.NewDecoder(r.Body).Decode(&args) != nil || args["role"] != secretstore.BackupRole || args["jwt"] != "backup-projection-fixture" {
			t.Fatal("snapshot did not use dedicated login")
		}
		body = `{"auth":{"client_token":"snapshot-session-fixture"}}`
	case "/v1/sys/storage/raft/snapshot":
		if r.Method != "GET" || r.Header.Get("X-Vault-Token") != "snapshot-session-fixture" {
			t.Fatal("snapshot used unexpected authority")
		}
		body = string(nativeSnapshotFixture(t))
	case "/v1/auth/token/revoke-self":
	default:
		t.Fatal("unexpected store request", r.URL.Path)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
}

func assertPrivateBackupContents(t *testing.T, path, identityPath string) {
	t.Helper()
	plain := privateBackupPlaintext(t, path, identityPath)
	buffer := bufio.NewReader(plain)
	line, err := buffer.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	_, boundary, _ := strings.Cut(strings.TrimSpace(line), " ")
	reader := multipart.NewReader(buffer, boundary)
	members := map[string][]byte{}
	for {
		part, err := reader.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		members[part.FileName()], err = io.ReadAll(part)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(members) != 5 || !bytes.Equal(members["store.snapshot"], nativeSnapshotFixture(t)) || len(members["database.tar"]) == 0 {
		t.Fatal("database or store snapshot missing")
	}
	for _, name := range []string{"oberth-ca", "oberth-admin", "oberth-forge-key", "oberth-ssh-host", "oberth-store-tls"} {
		if !bytes.Contains(members["installation-secrets.json"], []byte(name)) {
			t.Fatal("installation Secret missing", name)
		}
	}
}

func nativeSnapshotFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := storefake.Snapshot([]byte("snapshot-private-fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func privateBackupPlaintext(t *testing.T, path, identityPath string) io.Reader {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil || bytes.Contains(raw, []byte("snapshot-private-fixture")) || bytes.Contains(raw, []byte("tls.key")) {
		t.Fatal("private content was not encrypted", err)
	}
	identity, err := os.Open(identityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer identity.Close()
	identities, err := age.ParseIdentities(identity)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(raw), identities...)
	if err != nil {
		t.Fatal(err)
	}
	return plain
}

func TestInstallationBackupRejectsChangedClusterBeforePrivateReads(t *testing.T) {
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	ns, err := f.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = "different-cluster"
	if _, err = f.client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.client.ClearActions()
	database, options := backupFixture(t)
	_, err = f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Path: filepath.Join(t.TempDir(), "refused.age")})
	if err == nil {
		t.Fatal("foreign cluster was backed up")
	}
	for _, action := range f.client.Actions() {
		if action.GetResource().Resource == "secrets" {
			t.Fatal("private reads preceded cluster identity check")
		}
	}
}
