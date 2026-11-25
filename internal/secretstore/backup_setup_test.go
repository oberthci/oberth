package secretstore_test

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestBackupSetupUsesOnlyDelegatedPaths(t *testing.T) {
	backend := &fake.Store{Initialized: true}
	transport := snapshotTransport(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("X-Vault-Token") != "delegated-fixture" || (r.URL.Path != "/v1/sys/policies/acl/oberth-backup" && r.URL.Path != "/v1/auth/kubernetes/role/oberth-backup") {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("private refusal"))}, nil
		}
		return backend.RoundTrip(r)
	})
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if err = client.SetupBackup(t.Context(), []byte("delegated-fixture"), config()); err != nil {
		t.Fatal("backup setup required root or unrelated paths", err)
	}
	if len(backend.Resources) != 2 || backend.Resources["/auth/kubernetes/role/oberth-backup"]["audience"] != config().Audience {
		t.Fatal("delegated setup did not provision scoped backup access")
	}
	checkRepeatedBackupSetup(t, client, backend)
}

func checkRepeatedBackupSetup(t *testing.T, client *secretstore.Client, backend *fake.Store) {
	t.Helper()
	before := len(backend.Calls)
	if err := client.SetupBackup(t.Context(), []byte("delegated-fixture"), config()); err != nil {
		t.Fatal(err)
	}
	for _, call := range backend.Calls[before:] {
		if call.Method != "GET" {
			t.Fatal("repeated backup setup rewrote a role")
		}
	}
	backend.Resources["/auth/kubernetes/role/oberth-backup"]["audience"] = "different"
	if err := client.SetupBackup(t.Context(), []byte("delegated-fixture"), config()); err == nil {
		t.Fatal("backup setup overwrote a conflicting login")
	}
	if err := client.SetupBackup(t.Context(), []byte("wrong-fixture"), config()); err == nil || strings.Contains(err.Error(), "private refusal") {
		t.Fatal("unauthorized setup succeeded or exposed the store refusal", err)
	}
}
