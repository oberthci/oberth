package client

import (
	"crypto/x509"
	"github.com/oberthci/oberth/internal/apicontract"
)

// A saved identity belongs to its selected server and complete authority pool.
func bindProfileAuthority(o *Options, p Profile, dir string) error {
	if dir == "" || o.CAFile == "" || o.URL != "" && !apicontract.SameServerAddress(o.URL, p.ServerURL) {
		return nil
	}
	var roots [2]*x509.CertPool
	for i, path := range []string{profilePath(p.CAFile, dir, "ca.crt"), o.CAFile} {
		data, err := ReadPublic(path)
		if err != nil {
			return err
		}
		roots[i] = x509.NewCertPool()
		if !roots[i].AppendCertsFromPEM(data) {
			return fileFailure(path, "trust", nil)
		}
	}
	if !roots[0].Equal(roots[1]) {
		return failure("profile_ca", nil)
	}
	// An equivalent override must not select authority again when transport opens.
	o.CAFile = profilePath(p.CAFile, dir, "ca.crt")
	return nil
}
