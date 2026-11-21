package secretstore_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/runtimeconfig"
	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

type mountedInput struct {
	field string
	limit int
	ca    bool
}

func TestServerMountedFileTargets(t *testing.T) {
	inputs := []mountedInput{{"files.secret_store_ca", 64 << 10, true}, {"files.secret_store_token", 64 << 10, false}}
	for _, input := range inputs {
		for _, link := range []bool{false, true} {
			for _, shape := range []string{"missing", "directory", "empty", "oversized", "value", "limit"} {
				name := input.field + map[bool]string{false: "/file/", true: "/symlink/"}[link] + shape
				t.Run(name, func(t *testing.T) { checkServerFileTarget(t, input, shape, link) })
			}
		}
	}
}

func checkServerFileTarget(t *testing.T, input mountedInput, shape string, link bool) {
	t.Helper()
	files := validServerFiles(t)
	path := filepath.Join(t.TempDir(), "private-file-marker")
	target := path
	if link {
		target = filepath.Join(t.TempDir(), "target")
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	createMountedTarget(t, target, shape, input.limit)
	if input.ca {
		files.SecretStoreCA = path
	} else {
		files.SecretStoreToken = path
	}
	backend := &fake.Store{}
	writer, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend}, config(), files)
	if shape == "value" || shape == "limit" {
		if err != nil {
			t.Fatal("regular target within its limit refused", err)
		}
		writer.Client.Close()
	} else {
		assertMountedRefusal(t, err, input.field, shape)
	}
	if len(backend.Calls) != 0 {
		t.Fatal("file validation contacted the store")
	}
}

func validServerFiles(t *testing.T) secretstore.ServerFiles {
	t.Helper()
	directory := t.TempDir()
	files := secretstore.ServerFiles{SecretStoreCA: filepath.Join(directory, "ca"), SecretStoreToken: filepath.Join(directory, "token")}
	for _, path := range []string{files.SecretStoreCA, files.SecretStoreToken} {
		if err := os.WriteFile(path, []byte("fixture-file-data"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func createMountedTarget(t *testing.T, path, shape string, limit int) {
	t.Helper()
	switch shape {
	case "missing":
		return
	case "directory":
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
	default:
		value := []byte("fixture-file-data")
		switch shape {
		case "empty":
			value = nil
		case "oversized":
			value = make([]byte, limit+1)
		case "limit":
			value = make([]byte, limit)
		}
		if err := os.WriteFile(path, value, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func assertMountedRefusal(t *testing.T, err error, field, shape string) {
	t.Helper()
	expected := map[string]string{"missing": "missing", "directory": "not a regular file", "empty": "empty", "oversized": "larger than"}[shape]
	if err == nil {
		t.Fatal("invalid mounted target accepted")
	}
	if !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), expected) {
		t.Fatal("mounted refusal does not identify the field and state", err)
	}
	for _, private := range []string{"fixture-file-data", "private-file-marker", "open ", "stat "} {
		if strings.Contains(err.Error(), private) {
			t.Fatal("mounted refusal disclosed private data")
		}
	}
}

func replaceMountedLink(t *testing.T, path, target string) {
	t.Helper()
	next := filepath.Join(t.TempDir(), "next-link")
	if err := os.Symlink(target, next); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(next, path); err != nil {
		t.Fatal(err)
	}
}

func TestServerMountedLinksLoadReplacementTargets(t *testing.T) {
	files := validServerFiles(t)
	caLink := filepath.Join(t.TempDir(), "ca-link")
	tokenLink := filepath.Join(t.TempDir(), "token-link")
	replaceMountedLink(t, caLink, files.SecretStoreCA)
	replaceMountedLink(t, tokenLink, files.SecretStoreToken)
	linked := secretstore.ServerFiles{SecretStoreCA: caLink, SecretStoreToken: tokenLink}
	backend := &fake.Store{}
	connection := secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend}
	writer, _, err := secretstore.ServerWriter(t.Context(), connection, config(), linked)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Client.Close()
	replacement := validServerFiles(t)
	if err := os.WriteFile(replacement.SecretStoreCA, []byte("replacement-public-ca"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(replacement.SecretStoreToken, []byte("replacement-projection"), 0600); err != nil {
		t.Fatal(err)
	}
	replaceMountedLink(t, caLink, replacement.SecretStoreCA)
	replaceMountedLink(t, tokenLink, replacement.SecretStoreToken)
	if err := writer.Put(t.Context(), "step-one", "public/key", []byte("value")); err != nil {
		t.Fatal(err)
	}
	if backend.Resources["/auth/kubernetes/login"]["jwt"] != "replacement-projection" {
		t.Fatal("token replacement was not reopened")
	}
	refreshed, delivery, err := secretstore.ServerWriter(t.Context(), connection, config(), linked)
	if err != nil {
		t.Fatal(err)
	}
	defer refreshed.Client.Close()
	if delivery.CAPEM != "replacement-public-ca" {
		t.Fatal("CA link replacement was not loaded")
	}
}

func TestServerTokenLinkRefusesInvalidReplacementBeforeStoreCall(t *testing.T) {
	for _, shape := range []string{"missing", "directory", "empty", "oversized"} {
		t.Run(shape, func(t *testing.T) {
			files := validServerFiles(t)
			link := filepath.Join(t.TempDir(), "token-link")
			replaceMountedLink(t, link, files.SecretStoreToken)
			files.SecretStoreToken = link
			backend := &fake.Store{}
			writer, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{URL: "https://store.oberth.svc", Transport: backend}, config(), files)
			if err != nil {
				t.Fatal(err)
			}
			defer writer.Client.Close()
			target := filepath.Join(t.TempDir(), "private-file-marker")
			createMountedTarget(t, target, shape, 64<<10)
			replaceMountedLink(t, link, target)
			err = writer.Put(t.Context(), "step-one", "public/key", []byte("value"))
			assertMountedRefusal(t, err, "files.secret_store_token", shape)
			if len(backend.Calls) != 0 {
				t.Fatal("invalid token replacement reached store")
			}
		})
	}
}

func TestServerWriterWithoutConfigurationGivesSharedRefusal(t *testing.T) {
	_, _, err := secretstore.ServerWriter(t.Context(), secretstore.Connection{}, secretstore.Config{}, secretstore.ServerFiles{})
	reason, remedy := runtimeconfig.SecretStoreConfiguration()
	got := publicerr.Classify(err)
	if err == nil || got.Code() != publicerr.Unavailable || got.Reason() != reason || got.Remedy() != remedy {
		t.Fatal("server configuration refusal differs", err)
	}
}
