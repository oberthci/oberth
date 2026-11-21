package secretstore_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

func TestServerWriterReopensProjectedTokenAndUsesWriterRole(t *testing.T) {
	backend := &fake.Store{}
	files := secretstore.ServerFiles{SecretStoreCA: filepath.Join(t.TempDir(), "ca.crt"), SecretStoreToken: filepath.Join(t.TempDir(), "token")}
	if err := os.WriteFile(files.SecretStoreCA, []byte("public-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(files.SecretStoreToken, []byte("first-projection"), 0600); err != nil {
		t.Fatal(err)
	}
	writer, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend}, config(), files)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Client.Close()
	for _, jwt := range []string{"first-projection", "rotated-projection"} {
		if err := os.WriteFile(files.SecretStoreToken, []byte(jwt), 0600); err != nil {
			t.Fatal(err)
		}
		if err := writer.Put(t.Context(), "step-one", "public/key", []byte("private-body-marker")); err != nil {
			t.Fatal(err)
		}
		login := backend.Resources["/auth/kubernetes/login"]
		if login["role"] != config().WriterRole || login["jwt"] != jwt {
			t.Fatal("writer used a stale token or wrong role")
		}
	}
	checkServerWriterEffects(t, backend)
}

func checkServerWriterEffects(t *testing.T, backend *fake.Store) {
	t.Helper()
	value := backend.Resources["/oberth/steps/step-one/public/key"]["value"]
	if value != base64.StdEncoding.EncodeToString([]byte("private-body-marker")) || !backend.Revoked {
		t.Fatal("write or session revocation missing")
	}
	for _, call := range backend.Calls {
		if call.Method == "GET" {
			t.Fatal("server read a secret")
		}
	}
}

func TestServerWriterMountedFilesAreRequiredAndBounded(t *testing.T) {
	for _, test := range []mountedFileCase{
		{name: "absent"}, {name: "empty", size: 0}, {name: "large", size: 64<<10 + 1}, {name: "relative", size: 1, relative: true}, {name: "directory", directory: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			checkMountedFileRefusal(t, test)
		})
	}
}

func TestServerWriterAndStepFetchAgainstStoreFake(t *testing.T) {
	backend := &fake.Store{}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("server-projection"), nil }}
	if err := writer.Put(t.Context(), "step-one", "public/key", []byte("private-body-marker")); err != nil {
		t.Fatal(err)
	}
	claim := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"system:serviceaccount:oberth-runs:step-one"}`))
	values, err := client.Fetch(t.Context(), config(), []byte("header."+claim+".signature"), []string{"public/key"})
	if err != nil || string(values["public/key"]) != "private-body-marker" {
		t.Fatal("secret did not reach its step", err)
	}
	raw, _ := json.Marshal(backend.Calls)
	if strings.Contains(string(raw), "private-body-marker") {
		t.Fatal("fake call log leaked private bytes")
	}
}

func TestServerWriterRefusesSealedStoreWithoutWriting(t *testing.T) {
	backend := &fake.Store{Sealed: true}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("server-projection"), nil }}
	err = writer.Put(t.Context(), "step-one", "public/key", []byte("private-body-marker"))
	if err == nil || !strings.Contains(err.Error(), "An administrator runs oberth unseal") || strings.Contains(err.Error(), "private-body-marker") {
		t.Fatal("sealed refusal missing or private")
	}
	if backend.Resources["/oberth/steps/step-one/public/key"] != nil {
		t.Fatal("sealed store was written")
	}
}

type mountedFileCase struct {
	name                string
	size                int
	relative, directory bool
}

func checkMountedFileRefusal(t *testing.T, test mountedFileCase) {
	t.Helper()

	dir := t.TempDir()
	ca := filepath.Join(dir, "ca")
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(ca, []byte("ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if test.name != "absent" && !test.directory {
		if err := os.WriteFile(token, make([]byte, test.size), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if test.directory {
		token = dir
	}
	if test.relative {
		token = "token"
	}
	_, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc", Transport: &fake.Store{}}, config(), secretstore.ServerFiles{SecretStoreCA: ca, SecretStoreToken: token})
	if err == nil {
		t.Fatal("unsafe mounted file accepted")
	}
}

func TestWriterReportsStoredDespiteSessionRevokeFailure(t *testing.T) {
	backend := &fake.Store{Fail: "/auth/token/revoke-self"}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	writer := secretstore.Writer{Client: client, Config: config(), Token: func(context.Context) ([]byte, error) { return []byte("projection"), nil }}
	if err = writer.Put(t.Context(), "step-one", "public/key", []byte("private-body-marker")); err != nil {
		t.Fatal("stored write reported as refused", err)
	}
	if backend.Resources["/oberth/steps/step-one/public/key"]["value"] == nil {
		t.Fatal("success reported without storage")
	}
	attempted := false
	for _, call := range backend.Calls {
		if call.Path == "/auth/token/revoke-self" {
			attempted = true
		}
	}
	if !attempted {
		t.Fatal("session revocation was not attempted")
	}
}

func TestServerWriterRejectsNonPEMCA(t *testing.T) {
	files := validServerFiles(t)
	_, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc", ServerName: "store.oberth.svc"}, config(), files)
	if err == nil {
		t.Fatal("non-PEM CA accepted by real transport")
	}
	if !strings.Contains(err.Error(), "files.secret_store_ca: invalid trust configuration") {
		t.Fatal("CA field lost", err)
	}
	if strings.Contains(err.Error(), "fixture-file-data") {
		t.Fatal("CA content entered refusal")
	}
}

func TestServerWriterBadAddressPreservesOwnerRefusal(t *testing.T) {
	files := validServerFiles(t)
	_, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc/not-an-origin", ServerName: "store.oberth.svc"}, config(), files)
	if err == nil || !strings.Contains(err.Error(), "Secret store address is invalid.") || strings.Contains(err.Error(), "files.secret_store_ca") {
		t.Fatal("address refusal was replaced with a CA refusal", err)
	}
}
