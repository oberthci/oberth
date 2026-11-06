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
	var body []byte
	if strings.Contains(value, "-----BEGIN") {
		// Discard OpenSSL preambles and any BOM before the first PEM block.
		body = []byte(value[strings.Index(value, "-----BEGIN"):])
	} else {
		var err error
		body, err = os.ReadFile(value) // #nosec G304 -- operator-supplied trust anchor path.
		if err != nil {
			var pathErr *os.PathError
			if errors.As(err, &pathErr) {
				return nil, fmt.Errorf("read CA certificate file: %w", pathErr.Err)
			}
			return nil, errors.New("read CA certificate file failed")
		}
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(body) {
		return nil, errors.New("CA value contains no PEM certificate")
	}
	return pool, nil
}
