package app

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
)

const maximumAdminKeyBytes = 64 << 10

func TLSCertificateFingerprint(path string) (string, error) {
	// #nosec G304 -- the operator explicitly supplies the in-pod certificate path.
	body, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("app: read TLS certificate: %w", err)
	}
	block, _ := pem.Decode(body)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", errors.New("app: TLS certificate file does not begin with a certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", fmt.Errorf("app: parse TLS certificate: %w", err)
	}
	digest := sha256.Sum256(certificate.Raw)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(digest[:]), nil
}

func AuthorizedKeyFingerprint(path string) (string, error) {
	// #nosec G304 -- the operator explicitly selects a local public-key file.
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("app: open uplink public key: %w", err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return "", fmt.Errorf("app: inspect uplink public key: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maximumAdminKeyBytes {
		return "", errors.New("app: uplink public key must be a non-empty regular file no larger than 64 KiB")
	}
	return AuthorizedKeyFingerprintReader(file)
}

// AuthorizedKeyFingerprintReader reads exactly one bounded, bare authorized
// key. It is used by the in-pod bootstrap command when '-' selects stdin.
func AuthorizedKeyFingerprintReader(reader io.Reader) (string, error) {
	if reader == nil {
		return "", errors.New("app: uplink public key input is required")
	}
	body, err := io.ReadAll(io.LimitReader(reader, maximumAdminKeyBytes+1))
	if err != nil {
		return "", fmt.Errorf("app: read uplink public key: %w", err)
	}
	if len(body) == 0 || len(body) > maximumAdminKeyBytes {
		return "", errors.New("app: uplink public key must be non-empty and no larger than 64 KiB")
	}
	key, _, options, rest, err := ssh.ParseAuthorizedKey(body)
	if err != nil {
		return "", fmt.Errorf("app: parse uplink public key: %w", err)
	}
	if len(options) != 0 || strings.TrimSpace(string(rest)) != "" {
		return "", errors.New("app: uplink key input must contain exactly one public key without authorization options")
	}
	return ssh.FingerprintSHA256(key), nil
}

func ValidateUplinkIdentity(identity string) error {
	if identity == "" || identity != strings.TrimSpace(identity) || len(identity) > 255 || !strings.Contains(identity, "@") {
		return errors.New("app: uplink identity must have the form identity@host")
	}
	for _, character := range identity {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return errors.New("app: uplink identity contains whitespace or control characters")
		}
	}
	return nil
}
