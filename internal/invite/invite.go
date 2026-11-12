// Package invite defines the public enrollment binding and private handoff file.
package invite

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"net/netip"
	"net/url"
	"strings"
	"time"
	"unicode"
)

const DefaultLifetime = 24 * time.Hour
const MaximumLifetime = 7 * 24 * time.Hour

type Binding struct {
	ID        string    `json:"id"`
	Identity  string    `json:"identity"`
	ServerURL string    `json:"server_url"`
	CAPin     string    `json:"ca_pin"`
	Scope     []string  `json:"scope"`
	ExpiresAt time.Time `json:"expires_at"`
}
type File struct {
	Binding
	Secret string `json:"secret"`
}

func ValidateIdentity(identity string) error {
	if len(identity) > 255 || !strings.Contains(identity, "@") || strings.TrimSpace(identity) != identity {
		return errors.New("invite identity must have the form identity@host")
	}
	for _, r := range identity {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return errors.New("invite identity contains whitespace or control characters")
		}
	}
	return nil
}
func ValidatePin(pin string) error {
	raw, err := hex.DecodeString(pin)
	if err != nil || len(raw) != 32 || strings.ToLower(pin) != pin {
		return errors.New("CA pin must be 64 lowercase hex digits")
	}
	return nil
}
func ValidateURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "/mcp" || u.RawPath != "" {
		return errors.New("invite server URL must be an HTTPS /mcp URL without credentials, query or fragment")
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		return errors.New("invite server URL must use a DNS name, not an IP address")
	}
	return nil
}
func NewSecret() (string, string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", err
	}
	defer clear(raw)
	secret := base64.RawURLEncoding.EncodeToString(raw)
	digest, err := SecretDigest(secret)
	return secret, digest, err
}
func SecretDigest(secret string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(secret)
	if err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(raw) != secret {
		return "", errors.New("invalid invite credential")
	}
	defer clear(raw)
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:]), nil
}
