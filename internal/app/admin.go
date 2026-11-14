package app

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode"

	"golang.org/x/crypto/ssh"
)

const maximumAdminKeyBytes = 64 << 10

// AuthorizedKeyFingerprintReader reads exactly one bounded, bare authorized
// key for first-admin enrollment.
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
