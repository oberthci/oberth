package installer_test

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

// A bundled-store archive without its snapshot cannot be restored, so a refused
// backup role must fail the backup and leave every retained archive in place.
func TestBundledBackupWithoutServiceAccountFailsAndKeepsArchives(t *testing.T) {
	f := setup(t)
	installed := apply(t, f, prepare(t, f))
	f.client.PrependReactor("create", "serviceaccounts", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewNotFound(schema.GroupResource{Resource: "serviceaccounts"}, "oberth-backup")
	})
	f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
		t.Fatal("missing backup account contacted the store")
		return nil, nil, nil
	}
	database, options := backupFixture(t)
	dir, kept := retainedArchiveFixture(t)
	_, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Directory: dir, Keep: 1})
	checkStoreAccessRefusal(t, err, dir, kept)
}

func TestBundledBackupWithoutSnapshotRoleFailsAndKeepsArchives(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden, http.StatusNotFound} {
		f := setup(t)
		installed := apply(t, f, prepare(t, f))
		installSnapshotFixture(t, f)
		f.engine.OpenStore = func(context.Context, secretstore.Config) (*secretstore.Client, func(), error) {
			client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: snapshotHTTP(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != "/v1/auth/kubernetes/login" {
					t.Fatal("backup without role attempted a store snapshot", r.URL.Path)
				}
				return &http.Response{StatusCode: status, Header: http.Header{}, Body: io.NopCloser(strings.NewReader("private-store-response")), Request: r}, nil
			})})
			return client, func() {}, err
		}
		database, options := backupFixture(t)
		dir, kept := retainedArchiveFixture(t)
		_, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Directory: dir, Keep: 1})
		checkStoreAccessRefusal(t, err, dir, kept)
		if strings.Contains(err.Error(), "private-store-response") {
			t.Fatal("store diagnostic leaked into the refusal")
		}
	}
}

func retainedArchiveFixture(t *testing.T) (string, []string) {
	t.Helper()
	dir := t.TempDir()
	kept := []string{"oberth-backup-20260101T000000.000000000Z.age", "oberth-backup-20260101T000000.000000000Z.age.manifest.json"}
	for _, name := range kept {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("prior-restorable-archive"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, kept
}

func checkStoreAccessRefusal(t *testing.T, err error, dir string, kept []string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "Store snapshot access is missing") {
		t.Fatal("bundled backup without its store snapshot counted as good", err)
	}
	entries, readErr := os.ReadDir(dir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, kept) {
		t.Fatal("refused backup published an archive or pruned a restorable one", names)
	}
}
