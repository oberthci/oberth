package installer

// `oberth backup keygen`: the dedicated recipient key pair backup archives
// are encrypted to (#065). Before it, the operator had to find an openssl
// one-liner in the repository docs.

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	backupRecipientKeyName  = "recipient.key"
	backupRecipientCertName = "recipient.pem"
	backupRecipientBits     = 4096
)

// BackupRecipientPaths returns the default recipient private key and
// certificate: <client config dir>/backup/recipient.{key,pem}.
func BackupRecipientPaths() (key, cert string, err error) {
	root, err := ClientConfigDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(root, "backup")
	return filepath.Join(dir, backupRecipientKeyName), filepath.Join(dir, backupRecipientCertName), nil
}

// GenerateBackupRecipient writes a new RSA-4096 private key (0600) and a
// self-signed certificate for it (0644) into dir, creating dir 0700. It
// refuses to overwrite either file: archives already encrypted to an
// existing key would become unrecoverable.
func GenerateBackupRecipient(dir string) (keyPath, certPath string, err error) {
	keyPath, certPath = filepath.Join(dir, backupRecipientKeyName), filepath.Join(dir, backupRecipientCertName)
	for _, path := range []string{keyPath, certPath} {
		if _, statErr := os.Lstat(path); statErr == nil {
			return "", "", fmt.Errorf("%s already exists; archives encrypted to it need it, so it is never replaced", path)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", fmt.Errorf("create %s: %w", dir, err)
	}
	key, err := rsa.GenerateKey(rand.Reader, backupRecipientBits)
	if err != nil {
		return "", "", fmt.Errorf("generate RSA key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return "", "", err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "oberth-backup"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageKeyEncipherment,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return "", "", fmt.Errorf("create recipient certificate: %w", err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", "", err
	}
	if err := writeNewFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	clear(keyDER)
	if err := writeNewFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return "", "", errors.Join(err, os.Remove(keyPath))
	}
	return keyPath, certPath, nil
}

// backupKeyStoredName records that the operator confirmed a copy of the key
// exists off the machine (#072). It holds the SHA-256 of the certificate, so
// a new key pair needs a new confirmation.
const backupKeyStoredName = "recipient.stored"

func certificateDigest(certPath string) (string, error) {
	raw, err := os.ReadFile(certPath) // #nosec G304 -- the operator's recipient certificate.
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// BackupKeyStored reports whether the operator confirmed storing a copy of
// the private key that belongs to certPath.
func BackupKeyStored(certPath string) bool {
	want, err := certificateDigest(certPath)
	if err != nil {
		return false
	}
	have, err := os.ReadFile(filepath.Join(filepath.Dir(certPath), backupKeyStoredName)) // #nosec G304 -- fixed name beside the certificate.
	return err == nil && strings.TrimSpace(string(have)) == want
}

// MarkBackupKeyStored records the confirmation for certPath.
func MarkBackupKeyStored(certPath string) error {
	digest, err := certificateDigest(certPath)
	if err != nil {
		return err
	}
	path := filepath.Join(filepath.Dir(certPath), backupKeyStoredName)
	if err := os.WriteFile(path, []byte(digest+"\n"), 0o600); err != nil { // #nosec G306 -- public digest, owner-only like its directory.
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

func writeNewFile(path string, body []byte, mode os.FileMode) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode) // #nosec G304 -- operator-chosen recipient directory, created exclusively.
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	_, writeErr := file.Write(body)
	syncErr := file.Sync()
	closeErr := file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		_ = os.Remove(path)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
