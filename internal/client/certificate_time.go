package client

import (
	"crypto/x509"
	"os"
	"path/filepath"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/publicerr"
)

// CheckUplinkExpiry diagnoses the joined profile's lifetime before SSH connects.
func CheckUplinkExpiry(identity string, now time.Time) error {
	if filepath.Base(identity) != UplinkPushKeyFile {
		return nil
	}
	path := filepath.Join(filepath.Dir(identity), "admin.crt")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil
	}
	data, err := ReadPublic(path)
	if err != nil {
		return err
	}
	leaf, err := clientcert.ParseCertificate(data)
	if err != nil {
		return fileFailure(path, "pair", err)
	}
	return certificateTime(leaf, now)
}

func certificateTime(leaf *x509.Certificate, now time.Time) error {
	if now.Before(leaf.NotBefore) {
		return publicerr.New(publicerr.Forbidden, "Client certificate is not valid yet.", "Check your clock or use a currently valid client certificate.", nil)
	}
	if !now.Before(leaf.NotAfter) {
		problem := publicerr.New(publicerr.Forbidden, "Client certificate has expired.", "Ask an administrator for a new invitation and join again; oberth renew works only while the certificate is still valid.", nil)
		problem.Context = []string{"Certificate expiry: " + leaf.NotAfter.UTC().Format(time.RFC3339)}
		return problem
	}
	return nil
}
