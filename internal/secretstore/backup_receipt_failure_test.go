package secretstore_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestBackupReceiptWriteFailureNeverStartsGeneration(t *testing.T) {
	for _, mode := range []string{"missing-parent", "read-only-parent"} {
		t.Run(mode, func(t *testing.T) {
			backend := &backupQuorum{t: t, backend: &fake.Store{}, mode: "success"}
			client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			dir := filepath.Join(t.TempDir(), "receipt-parent")
			if mode == "read-only-parent" {
				if err = os.Mkdir(dir, 0555); err != nil {
					t.Fatal(err)
				}
			}
			err = client.SetupBackupWithUnseal(t.Context(), secretstore.BackupAuthorization{Config: config(), Share: []byte("saved-share-fixture"), Receipt: filepath.Join(dir, "pending.json")})
			if err == nil || publicerr.Classify(err).Reason() != "Authorization receipt cannot be saved." || backend.requests > 1 || backend.otp != "" || backend.granted {
				t.Fatal("receipt write failure started generation or asked for root cleanup", err)
			}
		})
	}
}
