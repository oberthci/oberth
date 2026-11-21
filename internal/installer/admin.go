package installer

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/pem"
	"math/big"
	"time"

	"filippo.io/age"
	"github.com/oberthci/oberth/internal/clientcert"
	"golang.org/x/crypto/ssh"
)

type admin struct {
	key, cert, public []byte
	id, action        string
}

func (e *Engine) administrator(i Identities, raw []byte) (result admin, err error) {
	defer func() {
		if err != nil {
			clear(raw)
		}
	}()
	raw, key, err := adminKey(e, raw)
	if err != nil {
		return admin{}, err
	}
	defer clear(key)
	ca, err := clientcert.ParseCertificate(i.CA.Data["tls.crt"])
	if err != nil {
		return admin{}, err
	}
	block, _ := pem.Decode(i.CA.Data["tls.key"])
	if block == nil {
		return admin{}, problem("identity", nil)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return admin{}, err
	}
	signer, ok := parsed.(*ecdsa.PrivateKey)
	if !ok {
		return admin{}, problem("identity", nil)
	}
	t := &x509.Certificate{
		SerialNumber: new(big.Int).SetBytes(key.Public().(ed25519.PublicKey)[:16]),
		NotBefore:    e.Now().Add(-time.Minute),
		NotAfter:     e.Now().AddDate(1, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	t.Subject.CommonName = "admin"
	der, err := x509.CreateCertificate(e.Random, t, ca, key.Public(), signer)
	if err != nil {
		clear(raw)
		return admin{}, err
	}
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		clear(raw)
		return admin{}, err
	}
	sum := sha256.Sum256(der)
	return admin{
		key:    raw,
		cert:   pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		public: ssh.MarshalAuthorizedKey(pub),
		id:     hex.EncodeToString(sum[:]),
	}, nil
}
func (e *Engine) backupKey() (string, error) {
	b, err := e.Credentials.read("backup.agekey")
	if err == nil {
		defer clear(b)
		key, err := age.ParseX25519Identity(string(b))
		if err != nil {
			return "", err
		}
		return key.Recipient().String(), nil
	}
	if !isAbsent(err) {
		return "", problem("custody", err)
	}
	// The age library uses its own cryptographic random source.
	key, err := age.GenerateX25519Identity()
	if err != nil {
		return "", err
	}
	raw := []byte(key.String())
	defer clear(raw)
	if err = e.Credentials.save("backup.agekey", raw); err != nil {
		return "", err
	}
	return key.Recipient().String(), nil
}

func adminKey(e *Engine, raw []byte) ([]byte, ed25519.PrivateKey, error) {
	if raw == nil {
		return keyPair(e.Random)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return raw, nil, problem("custody", nil)
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(ed25519.PrivateKey)
	if err != nil || !ok {
		return raw, nil, problem("custody", err)
	}
	return raw, key, nil
}
