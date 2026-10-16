package installer

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeEnvelopeCipher stands in for OpenSSL in unit tests: it prefixes the
// plaintext with a DER header naming AuthEnvelopedData, so the archive
// format, inventory and hash checks run without a host binary.
type fakeEnvelopeCipher struct{}

func fakeEnvelopeHeader() []byte {
	oid, err := asn1.Marshal(authEnvelopedDataOID)
	if err != nil {
		panic(err)
	}
	return append([]byte{0x30, 0x80}, oid...)
}

func (fakeEnvelopeCipher) Encrypt(_ context.Context, plaintext io.Reader, ciphertext io.Writer) error {
	if _, err := ciphertext.Write(fakeEnvelopeHeader()); err != nil {
		return err
	}
	_, err := io.Copy(ciphertext, plaintext)
	return err
}

func (fakeEnvelopeCipher) Decrypt(_ context.Context, ciphertextPath, plaintextPath string) error {
	body, err := os.ReadFile(ciphertextPath)
	if err != nil {
		return err
	}
	header := fakeEnvelopeHeader()
	if !bytes.HasPrefix(body, header) {
		return errors.New("not a fake envelope")
	}
	return os.WriteFile(plaintextPath, body[len(header):], 0o600)
}

// writeTestMembers fills dir with one small file per archive member.
func writeTestMembers(t *testing.T, dir string) {
	t.Helper()
	for _, name := range backupMemberOrder {
		if err := writeBackupFile(dir, name, []byte(`{"member":"`+name+`"}`)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestBackupArchiveRoundTripChecksEveryMember(t *testing.T) {
	t.Parallel()
	source := t.TempDir()
	writeTestMembers(t, source)
	output := filepath.Join(t.TempDir(), "state.cms")
	manifest := backupManifest{Format: backupFormat, CreatedAt: "2026-10-17T00:00:00Z", NotRestored: backupNotRestored}
	if err := writeBackupArchive(context.Background(), source, &manifest, fakeEnvelopeCipher{}, output); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(output)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("archive mode = %04o, want 0600", info.Mode().Perm())
	}
	if err := writeBackupArchive(context.Background(), source, &manifest, fakeEnvelopeCipher{}, output); err == nil {
		t.Fatal("second write over an existing archive succeeded; want refusal")
	}

	restored := t.TempDir()
	got, err := openBackupArchive(context.Background(), output, fakeEnvelopeCipher{}, restored)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != len(backupMemberOrder) {
		t.Fatalf("members = %d, want %d", len(got.Members), len(backupMemberOrder))
	}
	for _, name := range backupMemberOrder {
		body, err := os.ReadFile(filepath.Join(restored, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != `{"member":"`+name+`"}` {
			t.Fatalf("member %s = %q", name, body)
		}
	}
	if _, err := os.Stat(filepath.Join(restored, "archive.tar")); !os.IsNotExist(err) {
		t.Fatalf("decrypted tar stream left behind: %v", err)
	}

	// Flip one byte of a member body: its hash no longer matches the manifest.
	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	index := bytes.Index(body, []byte(`{"member":"`+backupMemberRoles))
	if index < 0 {
		t.Fatal("member body not found in fake envelope")
	}
	body[index+2] ^= 0x20
	tampered := filepath.Join(t.TempDir(), "tampered.cms")
	if err := os.WriteFile(tampered, body, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = openBackupArchive(context.Background(), tampered, fakeEnvelopeCipher{}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), backupMemberRoles) {
		t.Fatalf("tampered member error = %v, want a mismatch naming %s", err, backupMemberRoles)
	}
}

func TestOpenBackupArchiveRefusesUnexpectedEntries(t *testing.T) {
	t.Parallel()
	manifest, err := json.Marshal(backupManifest{Format: backupFormat})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*tar.Writer){
		"extra member": func(w *tar.Writer) {
			writeTarEntry(t, w, backupManifestName, manifest)
			writeTarEntry(t, w, "openbao/extra.json", []byte("{}"))
		},
		"member before manifest": func(w *tar.Writer) {
			writeTarEntry(t, w, backupMemberKV, []byte("{}"))
			writeTarEntry(t, w, backupManifestName, manifest)
		},
		"missing members": func(w *tar.Writer) {
			writeTarEntry(t, w, backupManifestName, manifest)
		},
	}
	for name, build := range cases {
		t.Run(name, func(t *testing.T) {
			var plaintext bytes.Buffer
			w := tar.NewWriter(&plaintext)
			build(w)
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "archive.cms")
			if err := os.WriteFile(path, append(fakeEnvelopeHeader(), plaintext.Bytes()...), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := openBackupArchive(context.Background(), path, fakeEnvelopeCipher{}, t.TempDir()); err == nil {
				t.Fatal("archive accepted; want refusal")
			}
		})
	}
}

func writeTarEntry(t *testing.T, w *tar.Writer, name string, body []byte) {
	t.Helper()
	if err := w.WriteHeader(&tar.Header{Name: name, Mode: 0o600, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(body); err != nil {
		t.Fatal(err)
	}
}

func TestRequireAuthEnvelopedDataRefusesOtherContent(t *testing.T) {
	t.Parallel()
	envelopedData, err := asn1.Marshal(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 3})
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string][]byte{
		"cbc enveloped data": append([]byte{0x30, 0x82, 0x01, 0x00}, envelopedData...),
		"plaintext tar":      []byte("manifest.json\x00\x00\x00"),
		"empty":              {},
	} {
		path := filepath.Join(t.TempDir(), "archive")
		if err := os.WriteFile(path, body, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := requireAuthEnvelopedData(path); err == nil {
			t.Fatalf("%s accepted; want refusal", name)
		}
	}
}

// testRecipient writes a self-signed recipient certificate and its key.
func testRecipient(t *testing.T, dir string, key any, public any) (certPath, keyPath string) {
	t.Helper()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "oberth-backup-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, public, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(dir, "recipient.pem")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	keyPath = filepath.Join(dir, "recipient.key")
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

func TestValidateBackupRecipientRequiresStrongRSA(t *testing.T) {
	t.Parallel()
	weak, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	weakCert, _ := testRecipient(t, t.TempDir(), weak, &weak.PublicKey)
	if err := validateBackupRecipient(weakCert); err == nil || !strings.Contains(err.Error(), "3072") {
		t.Fatalf("2048-bit recipient error = %v, want the 3072-bit floor", err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecCert, _ := testRecipient(t, t.TempDir(), ec, &ec.PublicKey)
	if err := validateBackupRecipient(ecCert); err == nil {
		t.Fatal("EC recipient accepted; want refusal (RSA-OAEP key transport)")
	}
}

func TestValidateBackupPrivateKeyRefusesSharedFile(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "key.pem")
	if err := os.WriteFile(path, []byte("-----BEGIN PRIVATE KEY-----\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	err := validateBackupPrivateKey(path)
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("group-readable key error = %v, want chmod advice", err)
	}
	if strings.Contains(err.Error(), "BEGIN PRIVATE KEY") {
		t.Fatal("error leaks key file contents")
	}
}

// TestBackupArchiveOpenSSLRoundTrip exercises the production cipher against
// the host's OpenSSL 3 when one is installed: AES-256-GCM CMS to a fresh
// RSA-3072 recipient, refusal of a wrong key, and refusal of a CBC envelope.
func TestBackupArchiveOpenSSLRoundTrip(t *testing.T) {
	binary, err := exec.LookPath("openssl")
	if err != nil {
		t.Skip("openssl not installed")
	}
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	keys := t.TempDir()
	certPath, keyPath := testRecipient(t, keys, key, &key.PublicKey)
	if err := validateBackupRecipient(certPath); err != nil {
		t.Fatal(err)
	}
	if err := validateBackupPrivateKey(keyPath); err != nil {
		t.Fatal(err)
	}

	source := t.TempDir()
	writeTestMembers(t, source)
	output := filepath.Join(t.TempDir(), "state.cms")
	manifest := backupManifest{Format: backupFormat}
	if err := writeBackupArchive(context.Background(), source, &manifest, opensslCMS{binary: binary, recipient: certPath}, output); err != nil {
		if strings.Contains(err.Error(), "aes-256-gcm") || strings.Contains(err.Error(), "cipher") {
			t.Skipf("host openssl lacks CMS AES-GCM: %v", err)
		}
		t.Fatal(err)
	}
	if _, err := openBackupArchive(context.Background(), output, opensslCMS{binary: binary, privateKey: keyPath}, t.TempDir()); err != nil {
		t.Fatal(err)
	}

	other, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		t.Fatal(err)
	}
	_, otherKey := testRecipient(t, t.TempDir(), other, &other.PublicKey)
	if _, err := openBackupArchive(context.Background(), output, opensslCMS{binary: binary, privateKey: otherKey}, t.TempDir()); err == nil {
		t.Fatal("archive opened with the wrong private key")
	}

	body, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	body[len(body)-40] ^= 0xff
	tampered := filepath.Join(t.TempDir(), "tampered.cms")
	if err := os.WriteFile(tampered, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openBackupArchive(context.Background(), tampered, opensslCMS{binary: binary, privateKey: keyPath}, t.TempDir()); err == nil {
		t.Fatal("tampered ciphertext accepted")
	}

	cbc := filepath.Join(t.TempDir(), "cbc.cms")
	command := exec.Command(binary, "cms", "-encrypt", "-binary", "-aes-256-cbc", "-outform", "DER", "-recip", certPath, "-out", cbc) // #nosec G204 -- test fixture
	command.Stdin = strings.NewReader("plaintext")
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build CBC fixture: %v: %s", err, out)
	}
	if err := requireAuthEnvelopedData(cbc); err == nil {
		t.Fatal("CBC EnvelopedData accepted; want refusal before decryption")
	}
}
