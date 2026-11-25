package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/installer"
)

func TestRestoreRejectsArchiveWithUnknownSigningKey(t *testing.T) {
	f := newRestoreFixture(t)
	f.config.Archive.Path = forgedRestoreArchive(t, f)
	_, err := f.target.engine.Restore(t.Context(), f.config)
	if err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatal("restore accepted an archive signed by the directory writer", err)
	}
	assertNoRestoreWrites(t, f.target)
	if len(f.target.charts.Applies) != 0 || f.storeApplied {
		t.Fatal("forged archive changed the installation")
	}
}

func TestRestorePinnedDigestRejectsReencryptedArchive(t *testing.T) {
	f := newRestoreFixture(t)
	f.config.Archive.Path = forgedRestoreArchive(t, f)
	f.config.Archive.ExpectedSHA256 = f.archive.SHA256
	if _, err := f.target.engine.Restore(t.Context(), f.config); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatal("restore ignored its independently saved digest", err)
	}
	assertNoRestoreWrites(t, f.target)
}

// The harness supplies valid plaintext to isolate sender authentication: changing
// encryption randomness and signing with another key must not establish origin.
func forgedRestoreArchive(t *testing.T, f *restoreFixture) string {
	t.Helper()
	identityRaw, err := os.ReadFile(f.config.Archive.IdentityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(identityRaw)
	identities, err := age.ParseIdentities(bytes.NewReader(identityRaw))
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(f.archive.Path)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := age.Decrypt(bytes.NewReader(original), identities...)
	if err != nil {
		t.Fatal(err)
	}
	var ciphertext bytes.Buffer
	writer, err := age.Encrypt(&ciphertext, identities[0].(*age.X25519Identity).Recipient())
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.Copy(writer, plain); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "forged.age")
	if err = os.WriteFile(path, ciphertext.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	forgeOriginManifest(t, f.archive.Path, path, sha256.Sum256(ciphertext.Bytes()))
	return path
}

func forgeOriginManifest(t *testing.T, source, target string, digest [32]byte) {
	t.Helper()
	raw, err := os.ReadFile(source + ".manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var proof struct {
		Origin    map[string]json.RawMessage `json:"origin"`
		Signature []byte                     `json:"signature"`
	}
	if err = json.Unmarshal(raw, &proof); err != nil {
		t.Fatal(err)
	}
	proof.Origin["archive_sha256"], err = json.Marshal(hex.EncodeToString(digest[:]))
	if err != nil {
		t.Fatal(err)
	}
	message, err := json.Marshal(proof.Origin)
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(private)
	proof.Signature = ed25519.Sign(private, append([]byte("oberth-backup-manifest/v1\n"), message...))
	raw, err = json.Marshal(proof)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(target+".manifest.json", raw, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupVerifyUsesOperatorKeyAndSavedDigestOffline(t *testing.T) {
	f := newRestoreFixture(t)
	if err := os.Remove(f.archive.Path + ".manifest.json"); err != nil {
		t.Fatal(err)
	}
	app, out, diagnostic := fixture(t, fake.Bindings())
	app.base = filepath.Join(t.TempDir(), "absent-profile")
	app.config.Transport = testTransport(func(*http.Request) (*http.Response, error) {
		t.Fatal("backup verification contacted a server")
		return nil, context.Canceled
	})
	args := []string{"backup", "verify", "--from", f.archive.Path, "--identity-file", f.config.Archive.IdentityPath, "--sha256", f.archive.SHA256, "--output", "json"}
	if code := app.run(t.Context(), args); code != 0 || diagnostic.Len() != 0 || !strings.Contains(out.String(), f.archive.SHA256) {
		t.Fatal("operator verification with saved digest failed", code, diagnostic)
	}
	config := f.config.Archive
	config.PublicKey = ""
	if _, err := installer.ReviewRestore(t.Context(), config); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatal("unsigned archive was accepted without a saved proof", err)
	}
}

// A genuine older archive renamed with its manifest must not pass as a newer one.
func TestRestoreRejectsGenuineArchiveUnderAnotherName(t *testing.T) {
	f := newRestoreFixture(t)
	if _, err := installer.ReviewRestore(t.Context(), f.config.Archive); err != nil {
		t.Fatal("genuine archive under its signed name was refused", err)
	}
	renamed := filepath.Join(filepath.Dir(f.archive.Path), "oberth-backup-20991231T000000.000000000Z.age")
	for _, suffix := range []string{"", ".manifest.json"} {
		if err := os.Rename(f.archive.Path+suffix, renamed+suffix); err != nil {
			t.Fatal(err)
		}
	}
	f.config.Archive.Path = renamed
	if _, err := installer.ReviewRestore(t.Context(), f.config.Archive); err == nil || !strings.Contains(err.Error(), "origin") {
		t.Fatal("signed manifest accepted a replayed archive under a newer name", err)
	}
	f.config.Archive.ExpectedSHA256 = f.archive.SHA256
	if _, err := installer.ReviewRestore(t.Context(), f.config.Archive); err != nil {
		t.Fatal("independently saved digest no longer selects a renamed copy", err)
	}
}
