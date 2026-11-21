package installer_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/x509"
	"testing"

	"github.com/oberthci/oberth/internal/clientcert"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInstallationCertificatesUseP256AndSHA256(t *testing.T) {
	t.Parallel()
	f := setup(t)
	apply(t, f, prepare(t, f))
	for _, name := range []string{"oberth-ca", "oberth-store-tls"} {
		s, err := f.client.CoreV1().Secrets("oberth").Get(t.Context(), name, meta.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		cert, err := clientcert.ParseCertificate(s.Data["tls.crt"])
		if err != nil {
			t.Fatal(err)
		}
		pub, ok := cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || pub.Curve != elliptic.P256() || cert.SignatureAlgorithm != x509.ECDSAWithSHA256 {
			t.Fatal("installation certificate requires P-256 and SHA-256", name)
		}
	}
}
