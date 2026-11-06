package client

import (
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
)

// LoadCAPool accepts an inline PEM or an operator-supplied PEM file path.
// Errors never interpolate inline certificate material.
func LoadCAPool(value string) (*x509.CertPool, error) {
	value = strings.TrimSpace(value)
	body := []byte(value)
	if !strings.HasPrefix(value, "-----BEGIN") {
		var err error
		body, err = os.ReadFile(value) // #nosec G304 -- operator-supplied trust anchor path.
		if err != nil {
			return nil, fmt.Errorf("read CA certificate file: %w", err)
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, errors.New("CA value contains no PEM certificate")
	}
	return pool, nil
}
