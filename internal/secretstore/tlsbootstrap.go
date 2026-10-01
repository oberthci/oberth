package secretstore

import (
	"bytes"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	tlsBundleDirectoryMode = 0o750
	tlsPublicFileMode      = 0o644
	tlsCAKeyFileMode       = 0o600
	tlsServerKeyFileMode   = 0o640
	maxTLSMaterialBytes    = 64 << 10
	openBaoTLSLifetime     = 10 * 365 * 24 * time.Hour
)

var openBaoTLSFiles = map[string]os.FileMode{
	"ca.crt":  tlsPublicFileMode,
	"ca.key":  tlsCAKeyFileMode,
	"tls.crt": tlsPublicFileMode,
	"tls.key": tlsServerKeyFileMode,
}

// BootstrapOpenBaoTLS creates or validates the private TLS bundle used by an
// installer-managed OpenBao listener. The directory is expected to live on
// OpenBao's durable PVC. Only the public CA certificate is returned; private
// key material is never emitted from the filesystem.
func BootstrapOpenBaoTLS(directory, namespace string) ([]byte, error) {
	cleanDirectory, err := validateTLSBootstrapInputs(directory, namespace)
	if err != nil {
		return nil, err
	}
	info, err := os.Lstat(cleanDirectory)
	switch {
	case err == nil:
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("OpenBao TLS path %s must be a real directory, not a link or non-directory", cleanDirectory)
		}
		return validateOpenBaoTLSBundle(cleanDirectory, namespace, time.Now())
	case !errors.Is(err, os.ErrNotExist):
		return nil, fmt.Errorf("inspect OpenBao TLS directory: %w", err)
	}
	return createOpenBaoTLSBundle(cleanDirectory, namespace, time.Now(), cryptorand.Reader)
}

func validateTLSBootstrapInputs(directory, namespace string) (string, error) {
	cleanDirectory := filepath.Clean(strings.TrimSpace(directory))
	if !filepath.IsAbs(cleanDirectory) || cleanDirectory == string(filepath.Separator) {
		return "", errors.New("OpenBao TLS output directory must be an absolute non-root path")
	}
	if !validDNSLabel(namespace) {
		return "", errors.New("OpenBao namespace must be a lowercase DNS label")
	}
	parent := filepath.Dir(cleanDirectory)
	resolvedParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return "", fmt.Errorf("resolve OpenBao TLS parent directory: %w", err)
	}
	if filepath.Clean(resolvedParent) != parent {
		return "", fmt.Errorf("OpenBao TLS parent directory %s must not traverse symbolic links", parent)
	}
	parentInfo, err := os.Stat(parent)
	if err != nil {
		return "", fmt.Errorf("inspect OpenBao TLS parent directory: %w", err)
	}
	if !parentInfo.IsDir() {
		return "", errors.New("OpenBao TLS parent path is not a directory")
	}
	return cleanDirectory, nil
}

func validDNSLabel(value string) bool {
	if value == "" || len(value) > 63 || value[0] == '-' || value[len(value)-1] == '-' {
		return false
	}
	for _, character := range value {
		if (character < 'a' || character > 'z') && (character < '0' || character > '9') && character != '-' {
			return false
		}
	}
	return true
}

func createOpenBaoTLSBundle(directory, namespace string, now time.Time, random io.Reader) (_ []byte, returnedError error) {
	caPublic, caPrivate, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, fmt.Errorf("generate OpenBao CA key: %w", err)
	}
	defer clearBytes(caPrivate)
	serverPublic, serverPrivate, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, fmt.Errorf("generate OpenBao server key: %w", err)
	}
	defer clearBytes(serverPrivate)

	caSerial, err := randomSerial(random)
	if err != nil {
		return nil, fmt.Errorf("generate OpenBao CA serial: %w", err)
	}
	notBefore := now.Add(-5 * time.Minute)
	notAfter := now.Add(openBaoTLSLifetime)
	caTemplate := &x509.Certificate{
		SerialNumber: caSerial,
		Subject: pkix.Name{
			CommonName:   "Oberth installer OpenBao CA",
			Organization: []string{"Oberth"},
		},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(random, caTemplate, caTemplate, caPublic, caPrivate)
	if err != nil {
		return nil, fmt.Errorf("create OpenBao CA certificate: %w", err)
	}

	serverSerial, err := randomSerial(random)
	if err != nil {
		return nil, fmt.Errorf("generate OpenBao server serial: %w", err)
	}
	serviceName := "openbao." + namespace + ".svc"
	serverTemplate := &x509.Certificate{
		SerialNumber: serverSerial,
		Subject:      pkix.Name{CommonName: serviceName, Organization: []string{"Oberth"}},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames: []string{
			"openbao",
			"openbao." + namespace,
			serviceName,
			serviceName + ".cluster.local",
			"localhost",
		},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}
	serverDER, err := x509.CreateCertificate(random, serverTemplate, caTemplate, serverPublic, caPrivate)
	if err != nil {
		return nil, fmt.Errorf("create OpenBao server certificate: %w", err)
	}

	caKeyDER, err := x509.MarshalPKCS8PrivateKey(caPrivate)
	if err != nil {
		return nil, fmt.Errorf("encode OpenBao CA key: %w", err)
	}
	defer clearBytes(caKeyDER)
	serverKeyDER, err := x509.MarshalPKCS8PrivateKey(serverPrivate)
	if err != nil {
		return nil, fmt.Errorf("encode OpenBao server key: %w", err)
	}
	defer clearBytes(serverKeyDER)
	material := map[string][]byte{
		"ca.crt":  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		"ca.key":  pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: caKeyDER}),
		"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		"tls.key": pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: serverKeyDER}),
	}
	defer clearBytes(material["ca.key"])
	defer clearBytes(material["tls.key"])

	parent := filepath.Dir(directory)
	staging, err := os.MkdirTemp(parent, ".oberth-tls-")
	if err != nil {
		return nil, fmt.Errorf("create OpenBao TLS staging directory: %w", err)
	}
	defer func() {
		if returnedError != nil {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, tlsBundleDirectoryMode); err != nil {
		return nil, fmt.Errorf("secure OpenBao TLS staging directory: %w", err)
	}
	for name, mode := range openBaoTLSFiles {
		if err := writeSyncedFile(filepath.Join(staging, name), material[name], mode); err != nil {
			return nil, err
		}
	}
	if err := syncDirectory(staging); err != nil {
		return nil, fmt.Errorf("sync OpenBao TLS staging directory: %w", err)
	}
	if err := os.Rename(staging, directory); err != nil {
		if info, statErr := os.Lstat(directory); statErr == nil && info.IsDir() {
			if cleanupErr := os.RemoveAll(staging); cleanupErr != nil {
				return nil, fmt.Errorf("remove unpublished OpenBao TLS staging directory: %w", cleanupErr)
			}
			return validateOpenBaoTLSBundle(directory, namespace, now)
		}
		return nil, fmt.Errorf("publish OpenBao TLS bundle: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return nil, fmt.Errorf("sync OpenBao TLS parent directory: %w", err)
	}
	return append([]byte(nil), material["ca.crt"]...), nil
}

func validateOpenBaoTLSBundle(directory, namespace string, now time.Time) ([]byte, error) {
	info, err := os.Stat(directory)
	if err != nil {
		return nil, fmt.Errorf("stat OpenBao TLS directory: %w", err)
	}
	if info.Mode().Perm() != tlsBundleDirectoryMode {
		return nil, fmt.Errorf("OpenBao TLS directory mode is %04o, want %04o", info.Mode().Perm(), os.FileMode(tlsBundleDirectoryMode))
	}
	material := make(map[string][]byte, len(openBaoTLSFiles))
	for name, mode := range openBaoTLSFiles {
		contents, err := readBoundedRegularFile(filepath.Join(directory, name), mode)
		if err != nil {
			return nil, err
		}
		material[name] = contents
	}
	defer clearBytes(material["ca.key"])
	defer clearBytes(material["tls.key"])

	ca, err := parseSingleCertificate(material["ca.crt"], "CA")
	if err != nil {
		return nil, err
	}
	if !ca.IsCA || ca.KeyUsage&x509.KeyUsageCertSign == 0 || ca.CheckSignatureFrom(ca) != nil {
		return nil, errors.New("OpenBao CA certificate is not a valid self-signed CA")
	}
	caPrivate, err := parseEd25519PrivateKey(material["ca.key"], "CA")
	if err != nil {
		return nil, err
	}
	defer clearBytes(caPrivate)
	caPublic, ok := ca.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(caPublic, caPrivate.Public().(ed25519.PublicKey)) {
		return nil, errors.New("OpenBao CA certificate and private key do not match")
	}

	server, err := parseSingleCertificate(material["tls.crt"], "server")
	if err != nil {
		return nil, err
	}
	serverPrivate, err := parseEd25519PrivateKey(material["tls.key"], "server")
	if err != nil {
		return nil, err
	}
	defer clearBytes(serverPrivate)
	serverPublic, ok := server.PublicKey.(ed25519.PublicKey)
	if !ok || !bytes.Equal(serverPublic, serverPrivate.Public().(ed25519.PublicKey)) {
		return nil, errors.New("OpenBao server certificate and private key do not match")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{
		Roots:       roots,
		CurrentTime: now,
		DNSName:     "openbao." + namespace + ".svc",
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		return nil, fmt.Errorf("verify OpenBao server certificate: %w", err)
	}
	if server.NotAfter.Before(now.Add(30 * 24 * time.Hour)) {
		return nil, errors.New("OpenBao server certificate expires within 30 days; rotate the installer-managed TLS bundle before upgrading")
	}
	return append([]byte(nil), material["ca.crt"]...), nil
}

func parseSingleCertificate(encoded []byte, label string) (*x509.Certificate, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("OpenBao %s certificate file must contain exactly one PEM certificate", label)
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse OpenBao %s certificate: %w", label, err)
	}
	return certificate, nil
}

func parseEd25519PrivateKey(encoded []byte, label string) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(encoded)
	if block == nil || block.Type != "PRIVATE KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("OpenBao %s private key file must contain exactly one PKCS#8 PEM key", label)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse OpenBao %s private key: %w", label, err)
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("OpenBao %s private key must use Ed25519", label)
	}
	return privateKey, nil
}

func randomSerial(random io.Reader) (*big.Int, error) {
	raw := make([]byte, 20)
	if _, err := io.ReadFull(random, raw); err != nil {
		return nil, err
	}
	raw[0] &= 0x7f
	serial := new(big.Int).SetBytes(raw)
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

func writeSyncedFile(path string, contents []byte, mode os.FileMode) error {
	// #nosec G304 -- path is one fixed bundle filename beneath the validated installer-owned staging directory.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return fmt.Errorf("create OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	failed := true
	defer func() {
		_ = file.Close()
		if failed {
			_ = os.Remove(path)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		return fmt.Errorf("set OpenBao TLS file mode for %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(contents); err != nil {
		return fmt.Errorf("write OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	failed = false
	return nil
}

func readBoundedRegularFile(path string, wantMode os.FileMode) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != wantMode || info.Size() <= 0 || info.Size() > maxTLSMaterialBytes {
		return nil, fmt.Errorf("OpenBao TLS file %s must be a non-empty bounded regular file with mode %04o", filepath.Base(path), wantMode)
	}
	file, err := os.Open(path) // #nosec G304 -- the directory is an installer-owned absolute PVC path validated above.
	if err != nil {
		return nil, fmt.Errorf("open OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil || !os.SameFile(info, openedInfo) || !openedInfo.Mode().IsRegular() {
		return nil, fmt.Errorf("OpenBao TLS file %s changed while being validated", filepath.Base(path))
	}
	contents, err := io.ReadAll(io.LimitReader(file, maxTLSMaterialBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read OpenBao TLS file %s: %w", filepath.Base(path), err)
	}
	if len(contents) > maxTLSMaterialBytes {
		return nil, fmt.Errorf("OpenBao TLS file %s exceeds the size bound", filepath.Base(path))
	}
	return contents, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path) // #nosec G304 -- caller supplies a validated installer-owned path.
	if err != nil {
		return err
	}
	defer func() { _ = directory.Close() }()
	return directory.Sync()
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
