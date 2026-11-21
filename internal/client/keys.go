package client

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"golang.org/x/crypto/ssh"
)

type localKeys struct {
	key, csr, pushKey, pushPublic []byte
	public                        ed25519.PublicKey
}

func generateKeys(identity string) (localKeys, error) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: identity}}, key)
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	_, push, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	private, err := ssh.MarshalPrivateKey(push, "")
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	sshPublic, err := ssh.NewPublicKey(push.Public())
	if err != nil {
		return localKeys{}, failure("keys", err)
	}
	return localKeys{pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), pem.EncodeToMemory(private), ssh.MarshalAuthorizedKey(sshPublic), public}, nil
}
