package secretstore

import (
	"strings"
	"testing"
)

func TestRequireVerifiedHTTPSRejectsDevelopmentHTTP(t *testing.T) {
	t.Parallel()
	client, err := New(Config{
		Address:           "http://127.0.0.1:8200",
		Role:              "oberth",
		AllowInsecureHTTP: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.requireVerifiedHTTPS("transit encrypt")
	if err == nil || !strings.Contains(err.Error(), "transit encrypt requires verified HTTPS") {
		t.Fatalf("error = %v", err)
	}
}

func TestRequireVerifiedHTTPSAcceptsVerifiedHTTPSClient(t *testing.T) {
	t.Parallel()
	client, err := New(Config{
		Address: "https://openbao.openbao.svc:8200",
		Role:    "oberth",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.requireVerifiedHTTPS("transit encrypt"); err != nil {
		t.Fatalf("verified HTTPS client rejected: %v", err)
	}
}
