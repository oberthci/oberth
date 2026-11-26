package main

import (
	"flag"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"github.com/oberthci/oberth/internal/secretstore"
)

func TestBackupSetupReadsMountedUnsealShare(t *testing.T) {
	mount := t.TempDir()
	version := filepath.Join(mount, "..2026_11_26")
	if err := os.Mkdir(version, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(version, "share"), []byte("share-fixture"), 0444); err != nil {
		t.Fatal(err)
	}
	for name, target := range map[string]string{"..data": "..2026_11_26", "share": "..data/share"} {
		if err := os.Symlink(target, filepath.Join(mount, name)); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chmod(mount, 0777); err != nil {
		t.Fatal(err)
	}
	flags := flag.NewFlagSet("setup", flag.ContinueOnError)
	flags.String("unseal-file", filepath.Join(mount, "share"), "")
	flags.String("token-file", "", "")
	flags.String("config", filepath.Join(t.TempDir(), "config.json"), "")
	cfg, err := decodeConfig(validConfig())
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	store, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: testTransport(func(*http.Request) (*http.Response, error) {
		calls++
		return &http.Response{StatusCode: 405, Body: http.NoBody}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	err = setupBackupWithUnseal(t.Context(), installationRequest{flags: flags}, store, storeConfiguration(cfg))
	if calls != 1 || publicerr.Classify(err).Reason() != "Store root generation is disabled." {
		t.Fatal("mounted share did not reach store authorization", calls, err)
	}
}
