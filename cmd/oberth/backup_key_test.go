package main

// The backup key is hard to lose and easy to find again (#072).

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeKeyPair writes an RSA key (0600) and its self-signed certificate.
func writeKeyPair(t *testing.T, keyPath, certPath, name string) (*rsa.PrivateKey, *x509.Certificate) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := x509.ParseCertificate(der)
	keyDER, _ := x509.MarshalPKCS8PrivateKey(key)
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if certPath != "" {
		if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return key, cert
}

// writeEnvelopeTo writes the CMS AuthEnvelopedData header of an archive
// encrypted (RSA-OAEP-SHA256) to cert's key.
func writeEnvelopeTo(t *testing.T, path string, key *rsa.PrivateKey, cert *x509.Certificate) {
	t.Helper()
	marshal := func(value any) []byte {
		out, err := asn1.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	explicit := func(body []byte) asn1.RawValue {
		return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: body}
	}
	rid := marshal(struct {
		Issuer asn1.RawValue
		Serial *big.Int
	}{asn1.RawValue{FullBytes: cert.RawIssuer}, cert.SerialNumber})
	params := marshal(struct{ Hash asn1.RawValue }{explicit(marshal(pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}}))})
	wrapped, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &key.PublicKey, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ktri := marshal(struct {
		Version   int
		RID       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Key       []byte
	}{0, asn1.RawValue{FullBytes: rid}, pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 7}, Parameters: asn1.RawValue{FullBytes: params}}, wrapped})
	envelope := marshal(struct {
		Version    int
		Recipients asn1.RawValue
		Content    asn1.RawValue
		MAC        []byte
	}{0, asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: ktri},
		asn1.RawValue{FullBytes: marshal(struct{ Type asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}})}, []byte("tag")})
	info := marshal(struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue
	}{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 23}, explicit(envelope)})
	if err := os.WriteFile(path, append(info, make([]byte, 512)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

// A restore uses the key that opens the backup, and names the one it needs
// when none here does.
func TestRestorePicksTheKeyThatOpensTheBackup(t *testing.T) {
	home := isolatedHome(t)
	defaultKey := filepath.Join(home, ".config", "oberth", "backup", "recipient.key")
	writeKeyPair(t, defaultKey, "", "this machine's key")
	carried := filepath.Join(home, "carried")
	archive := filepath.Join(carried, "oberth-backup-1.tar.gz.enc")
	sourceKey, sourceCert := writeKeyPair(t, filepath.Join(home, "elsewhere", "source.key"), "", "oberth backup recipient source")
	if err := os.MkdirAll(carried, 0o700); err != nil {
		t.Fatal(err)
	}
	writeEnvelopeTo(t, archive, sourceKey, sourceCert)

	_, err := resolveBackupKey("", archive)
	if !errors.Is(err, errUsage) {
		t.Fatalf("no matching key = %v", err)
	}
	for _, want := range []string{"no backup key opens", "CN=oberth backup recipient source, serial ", "recipient.key (a different key)", "carried/recipient.key (absent)"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	if _, err := resolveBackupKey(defaultKey, archive); err == nil || !strings.Contains(err.Error(), "does not open") {
		t.Fatalf("--key with the wrong key = %v", err)
	}
	// The right key next to the backup wins over the default one.
	beside := filepath.Join(carried, "recipient.key")
	source, err := os.ReadFile(filepath.Join(home, "elsewhere", "source.key"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(beside, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := resolveBackupKey("", archive); err != nil || got != beside {
		t.Fatalf("resolved %q, %v; want the key beside the backup", got, err)
	}
}

func TestFirstBackupAsksToStoreTheKey(t *testing.T) {
	home := isolatedHome(t)
	previous := backupInputIsTerminal
	backupInputIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { backupInputIsTerminal = previous })
	stored := filepath.Join(home, ".config", "oberth", "backup", "recipient.stored")

	// Three wrong answers cancel before anything is read from the cluster.
	var out bytes.Buffer
	err := runCLI(context.Background(), []string{"backup"}, strings.NewReader("yes\nok\ndone\n"), &out)
	if err == nil || !strings.Contains(err.Error(), "backup cancelled before anything was read") {
		t.Fatalf("unconfirmed backup = %v", err)
	}
	if _, statErr := os.Stat(stored); statErr == nil {
		t.Fatal("an unconfirmed key was recorded as stored")
	}
	for _, want := range []string{"!! Backup key  ~/.config/oberth/backup/recipient.key", "deleting ~/.config/oberth deletes it", "oberth backup key --print", "Type stored"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("warning lacks %q:\n%s", want, out.String())
		}
	}

	// "stored" records it; the backup then goes on (and fails here, with no cluster).
	out.Reset()
	err = runCLI(context.Background(), []string{"backup"}, strings.NewReader("stored\n"), &out)
	if err == nil || strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("confirmed backup = %v", err)
	}
	if _, statErr := os.Stat(stored); statErr != nil {
		t.Fatalf("the confirmation was not recorded: %v", statErr)
	}
	out.Reset()
	_ = runCLI(context.Background(), []string{"backup"}, strings.NewReader(""), &out)
	if strings.Contains(out.String(), "!! Backup key") {
		t.Fatalf("asked again for a confirmed key:\n%s", out.String())
	}
}

func TestBackupWithoutATerminalWarnsOnStderr(t *testing.T) {
	isolatedHome(t)
	var warning bytes.Buffer
	previous := backupStderr
	backupStderr = &warning
	t.Cleanup(func() { backupStderr = previous })
	err := runCLI(context.Background(), []string{"backup"}, strings.NewReader(""), &bytes.Buffer{})
	if err != nil && strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("a non-interactive backup was cancelled: %v", err)
	}
	if !strings.Contains(warning.String(), "!! Backup key") || !strings.Contains(warning.String(), "Not confirmed") {
		t.Fatalf("stderr warning = %q", warning.String())
	}
}

func TestBackupKeyCommand(t *testing.T) {
	isolatedHome(t)
	var out bytes.Buffer
	if err := runCLI(context.Background(), []string{"backup", "key"}, strings.NewReader(""), &out); !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "no backup key") {
		t.Fatalf("backup key without a key = %v", err)
	}
	if err := runCLI(context.Background(), []string{"backup", "keygen"}, strings.NewReader(""), &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := runCLI(context.Background(), []string{"backup", "key"}, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Backup key   ~/.config/oberth/backup/recipient.key", "CN=oberth-backup, serial ", "not confirmed yet"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("backup key output lacks %q:\n%s", want, out.String())
		}
	}
	out.Reset()
	err := runCLI(context.Background(), []string{"backup", "key", "--print"}, strings.NewReader(""), &out)
	if !errors.Is(err, errUsage) || !strings.Contains(err.Error(), "only on a terminal") || strings.Contains(out.String(), "PRIVATE KEY") {
		t.Fatalf("--print into a buffer = %v\n%s", err, out.String())
	}
}
