package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/stateport"
	"github.com/oberthci/oberth/internal/store"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type restoreFixture struct {
	source, target restoreCluster
	config         installer.RestoreConfig
	archive        installer.Archive
	destination    string
	storeApplied   bool
}

func setupClock() time.Time { return time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC) }

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := &restoreFixture{source: restoreClusterFixture(t), target: restoreClusterFixture(t), destination: filepath.Join(t.TempDir(), "restored.sqlite")}
	f.source.choices.Values = map[string]any{"runs": map[string]any{"parallel": 5}}
	installed := installRestoreSource(t, f.source)
	configureRestoreSnapshot(t, f.source)
	database, options := restoreDatabaseFixture(t)
	var err error
	f.archive, err = f.source.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Path: filepath.Join(t.TempDir(), "complete.age")})
	if err != nil {
		t.Fatal(err)
	}
	administrator, err := os.ReadFile(installed.Profile.AdminKey)
	if err != nil {
		t.Fatal(err)
	}
	unseal, err := os.ReadFile(installed.Profile.RecoveryKey)
	if err != nil {
		t.Fatal(err)
	}
	f.config = installer.RestoreConfig{Archive: installer.RestoreReviewConfig{Path: f.archive.Path, IdentityPath: installed.Profile.BackupKey, PublicKey: installed.Profile.BackupPublicKey, Inspector: database, Options: options}, Release: f.target.release,
		Choices: f.target.choices, Administrator: administrator, Unseal: unseal, Yes: true}
	f.config.Choices.URL = "https://restored.oberth.ci"
	t.Cleanup(func() { clear(administrator); clear(unseal) })
	ns, err := f.target.client.CoreV1().Namespaces().Get(t.Context(), "kube-system", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ns.UID = "fresh-cluster"
	if _, err = f.target.client.CoreV1().Namespaces().Update(t.Context(), ns, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	f.target.client.ClearActions()
	f.target.engine.RestoreDatabase = f.databaseTransfer(t, database)
	f.target.engine.OpenStore = f.storeTransport(t)
	f.target.engine.Pause = func(context.Context) error { return errors.New("restore waited without native progress") }
	return f
}

func restoreDatabaseFixture(t *testing.T) (*store.Backup, stateport.Options) {
	t.Helper()
	options := stateport.Options{Protocol: stateport.DefaultProtocol(), Schema: store.Schema}
	path := filepath.Join(t.TempDir(), "source.sqlite")
	database, err := store.Create(t.Context(), path, store.Options{Protocol: options.Protocol, Clock: setupClock})
	if err != nil {
		t.Fatal(err)
	}
	if err = database.RecordAdministration(t.Context(), stateport.Action{Actor: "admin"}, model.AuditAction{Actor: "admin", Action: "restore.fixture", ResourceType: "installation"}); err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Dir(store.HostPinsPath(path)), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(store.HostPinsPath(path), []byte(restorePinsFixture), 0600); err != nil {
		t.Fatal(err)
	}
	backup, err := store.NewBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	return backup, options
}

func (f *restoreFixture) databaseTransfer(t *testing.T, database *store.Backup) installer.DatabaseRestore {
	t.Helper()
	return func(ctx context.Context, namespace string, options stateport.Options, in io.Reader) (stateport.BackupReport, error) {
		if namespace != "oberth" || !f.storeApplied || len(f.target.charts.Applies) != 1 || f.target.charts.Applies[0].Values["restorePending"] != true {
			t.Fatal("database restore ran before a stopped installation and restored store")
		}
		return database.Restore(ctx, stateport.RestoreRequest{Options: options, Destination: f.destination}, in)
	}
}

func TestFreshClusterRestorePreservesDatabaseSecretsValuesAndStore(t *testing.T) {
	f := newRestoreFixture(t)
	result, err := f.target.engine.Restore(t.Context(), f.config)
	if err != nil {
		t.Fatal("fresh cluster restore failed", err)
	}
	if !result.Profile.Ready || result.Profile.ClusterUID != "fresh-cluster" || !f.storeApplied {
		t.Fatal("restore did not finish against the fresh cluster")
	}
	checkRestoredSecrets(t, f)
	checkRestoredDatabase(t, f)
	if len(f.target.charts.Applies) != 2 || f.target.charts.Applies[1].Values["restorePending"] != false || restoreJSON(t, f.target.charts.Applies[1].Values["runs"]) != `{"parallel":5}` {
		t.Fatal("restore lost saved values or started before both restores completed")
	}
	if strings.Contains(restoreJSON(t, result), "snapshot-private-fixture") || strings.Contains(restoreJSON(t, result), "PRIVATE KEY") {
		t.Fatal("restore result exposed private material")
	}
}

func checkRestoredSecrets(t *testing.T, f *restoreFixture) {
	t.Helper()
	for _, name := range []string{"oberth-admin", "oberth-ca", "oberth-ssh-host", "oberth-forge-key", "oberth-store-tls"} {
		source, err := f.source.client.CoreV1().Secrets("oberth").Get(t.Context(), name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		target, err := f.target.client.CoreV1().Secrets("oberth").Get(t.Context(), name, metav1.GetOptions{})
		if err != nil || !reflect.DeepEqual(source.Data, target.Data) || source.Type != target.Type {
			t.Fatal("restore changed an installation Secret", name, err)
		}
	}
}

// The server's forge host pins sit beside its database; the reviewed pins
// come back with it, so forge access works without a new upstream add.
const restorePinsFixture = "[forge.oberth.ci]:2222 ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIHostPinFixture\n"

func checkRestoredDatabase(t *testing.T, f *restoreFixture) {
	t.Helper()
	if pins, err := os.ReadFile(store.HostPinsPath(f.destination)); err != nil || string(pins) != restorePinsFixture {
		t.Fatal("restore lost the server's forge host pins", err)
	}
	restored, err := store.Open(t.Context(), f.destination, store.Options{Protocol: f.config.Archive.Options.Protocol, Clock: setupClock})
	if err != nil {
		t.Fatal("restored database cannot reopen", err)
	}
	defer restored.Close()
	rows, err := restored.AdministrativeAudit(t.Context(), nil, stateport.AdministrativeFilter{Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Action != "restore.fixture" || rows[0].SHA256 != f.archive.Report.AuditHead.SHA256 {
		t.Fatal("restore lost schema 6 database content or its audit head", err)
	}
}

func (f *restoreFixture) storeTransport(t *testing.T) func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
	t.Helper()
	initialized, sealed := false, false
	return func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: testTransport(func(r *http.Request) (*http.Response, error) {
			body, status := "", http.StatusOK
			switch r.URL.Path {
			case "/v1/sys/seal-status":
				body = fmtStoreStatus(initialized, sealed)
			case "/v1/sys/init":
				initialized, sealed = true, true
				body = `{"keys_base64":["temporary-share"],"root_token":"temporary-target-admin"}`
			case "/v1/sys/unseal":
				var request map[string]string
				expected := "temporary-share"
				if f.storeApplied {
					expected = string(f.config.Unseal)
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil || request["key"] != expected {
					t.Fatal("restore did not use the target share followed by the original unseal material")
				}
				sealed = false
				body = `{"sealed":false}`
			case "/v1/sys/health":
			case "/v1/sys/storage/raft/snapshot-force":
				raw, err := io.ReadAll(r.Body)
				if err != nil || !bytes.Equal(raw, nativeSnapshotFixture(t)) || r.Header.Get("X-Vault-Token") != "temporary-target-admin" {
					t.Fatal("restore did not transfer exact native store state under temporary target authority")
				}
				f.storeApplied, sealed, status = true, true, http.StatusNoContent
			default:
				t.Fatal("unexpected restore store operation", r.URL.Path)
			}
			return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body))}, nil
		})})
		return client, func() {}, err
	}
}

func fmtStoreStatus(initialized, sealed bool) string {
	if !initialized {
		return `{"initialized":false,"sealed":false}`
	}
	if sealed {
		return `{"initialized":true,"sealed":true}`
	}
	return `{"initialized":true,"sealed":false}`
}
