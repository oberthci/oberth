package installer_test

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/secretstore"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ktesting "k8s.io/client-go/testing"
)

func TestExistingStoreBackupWithoutServiceAccount(t *testing.T) {
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
	archive, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Path: filepath.Join(t.TempDir(), "before-upgrade.age")})
	if err != nil || archive.SHA256 == "" || !strings.Contains(archive.StoreSnapshotHint, "Store snapshot not included") {
		t.Fatal("old installation could not back up without its new account", err)
	}
}

func TestExistingStoreBackupKeepsContentsWithoutSnapshotRole(t *testing.T) {
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
		path := filepath.Join(t.TempDir(), "existing-store.age")
		archive, err := f.engine.BackupInstallation(t.Context(), installer.BackupConfig{Source: database, Options: options, Recipient: installed.Profile.BackupRecipient, Path: path})
		if err != nil || !strings.Contains(archive.StoreSnapshotHint, "Store snapshot not included") || !strings.Contains(archive.StoreSnapshotHint, "stage 2") {
			t.Fatal("existing store could not back up remaining installation contents", err, archive.StoreSnapshotHint)
		}
		f.client.ClearActions()
		review, err := installer.ReviewRestore(t.Context(), installer.RestoreReviewConfig{Path: path, IdentityPath: installed.Profile.BackupKey, Inspector: database, Options: options})
		if err != nil || review.StoreIncluded || review.Secrets != 5 || review.Database != archive.Report || len(f.client.Actions()) != 0 {
			t.Fatal("backup omission was dishonest or lost installation contents", review, err)
		}
		if strings.Contains(jsonOf(t, archive)+jsonOf(t, review), "private-store-response") {
			t.Fatal("store diagnostic leaked into backup output")
		}
	}
}
