package installer

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"

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
