package secretstore_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestUnfinishedBackupGenerationNeedsOnlyCancellation(t *testing.T) {
	for _, mode := range []string{"wrong-share", "wrong-share-400", "init-loss", "unfinished", "in-progress"} {
		t.Run(mode, func(t *testing.T) { checkUnfinishedGeneration(t, mode) })
	}
}

func checkUnfinishedGeneration(t *testing.T, mode string) {
	t.Helper()
	backend := &backupQuorum{t: t, backend: &fake.Store{}, mode: mode}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	path := filepath.Join(t.TempDir(), "pending.json")
	err = client.SetupBackupWithUnseal(t.Context(), secretstore.BackupAuthorization{Config: config(), Share: []byte("saved-share-fixture"), Receipt: path})
	if err == nil || backend.granted {
		t.Fatal("fixture must fail without issuing authority")
	}
	raw, marshalErr := json.Marshal(err)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(raw), "revoke-accessor") || !strings.Contains(string(raw), "DELETE sys/generate-root/attempt") {
		t.Fatal("unfinished generation must require cancellation alone")
	}
	if strings.Contains(string(raw), "private-share-refusal") {
		t.Fatal("native share rejection exposed private response text")
	}
	if _, err = os.Lstat(path); !os.IsNotExist(err) {
		t.Fatal("definite non-issuance left a receipt requiring unrelated accessor cleanup", err)
	}
	backend.mode = "in-progress"
	err = client.SetupBackupWithUnseal(t.Context(), secretstore.BackupAuthorization{Config: config(), Share: []byte("saved-share-fixture"), Receipt: path})
	raw, _ = json.Marshal(err)
	if err == nil || strings.Contains(string(raw), "revoke-accessor") {
		t.Fatal("next setup required accessor cleanup for unfinished generation")
	}
}

func TestUnknownBackupGenerationRetainsAccessorCleanup(t *testing.T) {
	for _, mode := range []string{"reply-loss", "update-unavailable", "update-400", "truncated-completion", "revoke-failed"} {
		t.Run(mode, func(t *testing.T) {
			backend := &backupQuorum{t: t, backend: &fake.Store{}, mode: mode}
			client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			path := filepath.Join(t.TempDir(), "pending.json")
			err = client.SetupBackupWithUnseal(t.Context(), secretstore.BackupAuthorization{Config: config(), Share: []byte("saved-share-fixture"), Receipt: path})
			if err == nil {
				t.Fatal("unknown issuance or revocation succeeded")
			}
			checkPendingReceipt(t, backend, path)
			checkPendingInstructions(t, backend, path, publicerr.Classify(err))
			calls := backend.requests
			err = client.SetupBackupWithUnseal(t.Context(), secretstore.BackupAuthorization{Config: config(), Share: []byte("saved-share-fixture"), Receipt: path})
			if backend.requests != calls {
				t.Fatal("uncertain outcome permitted another issuing request")
			}
			checkPendingInstructions(t, backend, path, publicerr.Classify(err))
		})
	}
}
