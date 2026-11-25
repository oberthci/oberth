package installer

import (
	"crypto/x509"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/clientcert"
)

func profileCAPool(p Profile) (*x509.CertPool, error) {
	raw, err := readProfileCA(p, os.ReadFile)
	if err != nil {
		return nil, err
	}
	cert, err := clientcert.ParseCertificate(raw)
	if err != nil || clientcert.Pin(cert) != p.CAPin {
		return nil, problem("tls", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(cert)
	return pool, nil
}

func readProfileCA(p Profile, read func(string) ([]byte, error)) ([]byte, error) {
	path := p.CAFile
	if path == "" {
		return nil, problem("credential-missing", nil)
	}
	raw, err := read(path)
	if isAbsent(err) {
		return nil, described("credential-missing", err, path)
	}
	if err != nil {
		return nil, described("custody", err, path)
	}
	return raw, nil
}

func (c *ConfigDir) profileCA() ([]byte, error) {
	p, err := c.profile()
	if err != nil {
		return nil, problem("custody", err)
	}
	return readProfileCA(p, func(path string) ([]byte, error) {
		if filepath.Dir(path) != c.path {
			return nil, problem("custody", nil)
		}
		return c.read(filepath.Base(path))
	})
}
