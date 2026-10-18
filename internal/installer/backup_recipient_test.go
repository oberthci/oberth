package installer

// Which key opens a backup (#072).

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// keyPairFixture is a small RSA key pair with a self-signed certificate,
// written as recipient.key (0600) and recipient.pem in dir.
type keyPairFixture struct {
	key               *rsa.PrivateKey
	cert              *x509.Certificate
	keyPath, certPath string
}

func newKeyPairFixture(t *testing.T, dir, name string) keyPairFixture {
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
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	r := keyPairFixture{key: key, cert: cert, keyPath: filepath.Join(dir, name+".key"), certPath: filepath.Join(dir, name+".pem")}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
	return r
}

// writeTestEnvelope writes the CMS AuthEnvelopedData header oberth backup
// writes (RSA-OAEP-SHA256 key transport to r), followed by filler.
func writeTestEnvelope(t *testing.T, path string, r keyPairFixture, oaep bool) {
	t.Helper()
	marshal := func(value any) []byte {
		out, err := asn1.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	rid := marshal(struct {
		Issuer asn1.RawValue
		Serial *big.Int
	}{asn1.RawValue{FullBytes: r.cert.RawIssuer}, r.cert.SerialNumber})
	algorithm := pkix.AlgorithmIdentifier{Algorithm: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}}
	if oaep {
		hash := marshal(pkix.AlgorithmIdentifier{Algorithm: oidSHA256})
		params := marshal(struct{ Hash asn1.RawValue }{asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: hash}})
		algorithm = pkix.AlgorithmIdentifier{Algorithm: oidRSAESOAEP, Parameters: asn1.RawValue{FullBytes: params}}
	}
	encryptedKey, err := rsa.EncryptOAEP(sha256.New(), rand.Reader, &r.key.PublicKey, make([]byte, 32), nil)
	if err != nil {
		t.Fatal(err)
	}
	ktri := marshal(struct {
		Version      int
		RID          asn1.RawValue
		Algorithm    pkix.AlgorithmIdentifier
		EncryptedKey []byte
	}{0, asn1.RawValue{FullBytes: rid}, algorithm, encryptedKey})
	envelope := marshal(struct {
		Version    int
		Recipients asn1.RawValue
		Content    asn1.RawValue
		MAC        []byte
	}{0, asn1.RawValue{Tag: asn1.TagSet, IsCompound: true, Bytes: ktri},
		asn1.RawValue{FullBytes: marshal(struct{ Type asn1.ObjectIdentifier }{asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}})}, []byte("tag")})
	// A RawValue is written verbatim, so the [0] wrapper is built by hand.
	info := marshal(struct {
		Type    asn1.ObjectIdentifier
		Content asn1.RawValue
	}{authEnvelopedDataOID, asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: envelope}})
	if err := os.WriteFile(path, append(info, make([]byte, 1024)...), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestArchiveRecipientsNameTheKeyItNeeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	mine, other := newKeyPairFixture(t, dir, "mine"), newKeyPairFixture(t, dir, "other")
	archive := filepath.Join(dir, "oberth-backup.tar.gz.enc")
	writeTestEnvelope(t, archive, mine, true)

	recipients, err := ArchiveRecipients(archive)
	want, certErr := BackupRecipientOf(mine.certPath)
	if err != nil || certErr != nil || len(recipients) != 1 || recipients[0] != want {
		t.Fatalf("recipients %v (%v), want %v (%v)", recipients, err, want, certErr)
	}
	if want.Issuer != "CN=mine" || want.Serial == "" {
		t.Fatalf("certificate identity %+v", want)
	}
	if opens, err := BackupKeyOpens(archive, mine.keyPath); !opens || err != nil {
		t.Fatalf("the recipient's key: opens %v, %v", opens, err)
	}
	if opens, err := BackupKeyOpens(archive, other.keyPath); opens || err != nil {
		t.Fatalf("another key: opens %v, %v; want a plain no", opens, err)
	}
	garbage := filepath.Join(dir, "garbage.key")
	if err := os.WriteFile(garbage, []byte("not a key"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := BackupKeyOpens(archive, garbage); !IsUncheckableKey(err) {
		t.Fatalf("an unparseable key = %v, want uncheckable (left to OpenSSL)", err)
	}
	pkcs1 := filepath.Join(dir, "pkcs1.key")
	if err := os.WriteFile(pkcs1, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(mine.key)}), 0o600); err != nil {
		t.Fatal(err)
	}
	if opens, err := BackupKeyOpens(archive, pkcs1); !opens || err != nil {
		t.Fatalf("the recipient's PKCS#1 key: opens %v, %v", opens, err)
	}

	legacy := filepath.Join(dir, "legacy.cms")
	writeTestEnvelope(t, legacy, mine, false)
	if _, err := BackupKeyOpens(legacy, mine.keyPath); !IsUncheckableKey(err) {
		t.Fatalf("a non-OAEP transport = %v, want uncheckable", err)
	}
	plain := filepath.Join(dir, "plain")
	if err := os.WriteFile(plain, []byte("not an archive"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ArchiveRecipients(plain); err == nil {
		t.Fatal("a non-CMS file named recipients")
	}
}

func TestBackupKeyStoredFollowsTheCertificate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	r := newKeyPairFixture(t, dir, "recipient")
	if BackupKeyStored(r.certPath) {
		t.Fatal("a fresh key counts as stored")
	}
	if err := MarkBackupKeyStored(r.certPath); err != nil {
		t.Fatal(err)
	}
	if !BackupKeyStored(r.certPath) {
		t.Fatal("the confirmation was not recorded")
	}
	// A new key pair in the same place needs its own confirmation.
	replacement := newKeyPairFixture(t, t.TempDir(), "recipient")
	body, err := os.ReadFile(replacement.certPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(r.certPath, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if BackupKeyStored(r.certPath) {
		t.Fatal("a confirmation carried over to a different key")
	}
}
