package client

import (
	"crypto/x509"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestSSHUplinkExpiryUsesCertificateRemedy(t *testing.T) {
	cert, _ := certificate(t, 1)
	leaf, err := x509.ParseCertificate(mustPEM(t, cert))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(filepath.Dir(privateFile(t, "admin.crt", cert)), UplinkPushKeyFile)
	if err := CheckUplinkExpiry(path, leaf.NotAfter.Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	err = CheckUplinkExpiry(path, leaf.NotAfter)
	if err == nil {
		t.Fatal("SSH uplink outlived its certificate")
	}
	p := publicerr.Classify(err)
	if !strings.Contains(p.Remedy(), "new invitation") || !strings.Contains(p.Remedy(), "oberth renew") || !strings.Contains(p.Remedy(), "join again") {
		t.Fatal("expiry remedy lost", p)
	}
}
