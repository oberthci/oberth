package releaseverify

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"testing"
)

func TestCanonicalPublicKeyUsesVerifierKeyType(t *testing.T) {
	private, public := signingKey(t)
	canonical, err := CanonicalPublicKey(append([]byte("\n "), public...))
	if err != nil || !bytes.Equal(canonical, public) {
		t.Fatal("public key did not normalize", err)
	}
	if _, err = parseKey(canonical); err != nil {
		t.Fatal("provisioned key is not accepted by verifier", err)
	}
	secret, err := x509.MarshalECPrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	other, err := x509.MarshalPKIXPublicKey(&rsa.PublicKey{N: big.NewInt(3233), E: 17})
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"private":       pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: secret}),
		"other type":    pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: other}),
		"leading text":  append([]byte("unexpected\n"), public...),
		"trailing text": append(bytes.Clone(public), []byte("unexpected")...),
		"second key":    append(bytes.Clone(public), public...),
		"empty":         nil,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalPublicKey(raw); !errors.Is(err, Key) {
				t.Fatal("non-public-P256 input accepted", err)
			}
		})
	}
}

func TestCanonicalOwnerPublicKeyFile(t *testing.T) {
	raw, err := os.ReadFile("release-cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) == 0 {
		return
	}
	canonical, err := CanonicalPublicKey(raw)
	if err != nil || !bytes.Equal(raw, canonical) {
		t.Fatal("owner pin is not canonical public PEM", err)
	}
}
