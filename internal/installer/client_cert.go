package installer

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/url"
	"os"
	"syscall"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
)

func writeAdminCertificate(path string, output, csrPEM []byte) error {
	block, _ := pem.Decode(output)
	if block == nil || block.Type != "CERTIFICATE" {
		return errors.New("installer did not receive a client certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return errors.New("installer received invalid client certificate")
	}
	csr, err := clientcert.ParseCSR(csrPEM)
	if err != nil {
		return err
	}
	if !bytes.Equal(csr.RawSubjectPublicKeyInfo, cert.RawSubjectPublicKeyInfo) || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		return errors.New("client certificate does not match admin CSR or usage")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600) //nolint:gosec // G304: certificate output next to the selected CSR; exclusive creation.
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.Write(pem.EncodeToMemory(block)); err != nil {
		return err
	}
	return file.Sync()
}

// Reuse only a bounded, regular public certificate bound to the selected CSR
// key and uplink identity. This is recovery of local enrollment output, not a
// claim that the selected server trusts that certificate or grants it access.
func reuseAdminCertificate(cfg *Config) error {
	file, err := os.OpenFile(cfg.AdminCSRPath+".crt", os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0) //nolint:gosec // G304: public certificate beside the operator-selected CSR; never written here.
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("cannot open existing public certificate without following links")
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("existing public certificate must be a regular file")
	}
	raw, err := io.ReadAll(io.LimitReader(file, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return errors.New("cannot read existing public certificate (maximum 64 KiB)")
	}
	cert, err := matchingAdminCertificate(raw, cfg.adminCSR)
	if err != nil {
		return err
	}
	identity := cert.Subject.CommonName
	if validateUplinkIdentity(identity) != nil || (cfg.UplinkIdentity != "" && cfg.UplinkIdentity != identity) {
		return errors.New("existing public certificate does not match the admin identity")
	}
	expected := (&url.URL{Scheme: "oberth", Host: "uplink", Path: "/" + identity}).String()
	if len(cert.URIs) != 1 || cert.URIs[0].String() != expected {
		return errors.New("existing public certificate does not name the admin uplink")
	}
	now := time.Now()
	if now.Before(cert.NotBefore) || !now.Before(cert.NotAfter) {
		return errors.New("existing public certificate is expired or not yet valid; use documented certificate renewal or enrollment")
	}
	cfg.UplinkIdentity = identity
	cfg.uplinkOnboarded = true
	return nil
}

func matchingAdminCertificate(raw, csrPEM []byte) (*x509.Certificate, error) {
	block, rest := pem.Decode(raw)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("expected one existing public client certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, errors.New("existing public certificate is invalid")
	}
	csr, err := clientcert.ParseCSR(csrPEM)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(csr.RawSubjectPublicKeyInfo, cert.RawSubjectPublicKeyInfo) || cert.IsCA || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth || len(cert.UnknownExtKeyUsage) != 0 || cert.KeyUsage != x509.KeyUsageDigitalSignature {
		return nil, errors.New("existing public certificate does not match the admin CSR key or client authentication usage")
	}
	return cert, nil
}
