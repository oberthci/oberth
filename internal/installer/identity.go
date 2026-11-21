package installer

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"time"

	"github.com/oberthci/oberth/internal/clientcert"
	"golang.org/x/crypto/ssh"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Identities are private buffers owned by one apply, outside chart revisions.
type Identities struct{ CA, Host, Forge, Store *corev1.Secret }

func (i Identities) clear() {
	for _, s := range []*corev1.Secret{i.CA, i.Host, i.Forge, i.Store} {
		if s != nil {
			for _, b := range s.Data {
				clear(b)
			}
		}
	}
}
func keyPair(random io.Reader) ([]byte, ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(random)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		clear(key)
		return nil, nil, err
	}
	defer clear(der)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), key, nil
}

type certificateSpec struct {
	ca     bool
	host   string
	parent *corev1.Secret
}

func certificate(random io.Reader, now time.Time, spec certificateSpec) (map[string][]byte, error) {
	keyPEM, key, err := certificateKey(random)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(random, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		clear(keyPEM)
		return nil, err
	}
	t := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: spec.host},
		NotBefore:             now.Add(-time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  spec.ca,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageDigitalSignature,
	}
	signer, issuer := key, t
	if spec.ca {
		t.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		t.MaxPathLenZero = true
	} else {
		t.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		if ip := net.ParseIP(spec.host); ip != nil {
			t.IPAddresses = []net.IP{ip}
		} else {
			t.DNSNames = []string{spec.host}
		}
		issuer, err = clientcert.ParseCertificate(spec.parent.Data["tls.crt"])
		if err != nil {
			return nil, err
		}
		t.NotAfter = issuer.NotAfter
		pair, e := tls.X509KeyPair(spec.parent.Data["tls.crt"], spec.parent.Data["tls.key"])
		if e != nil {
			return nil, e
		}
		var ok bool
		signer, ok = pair.PrivateKey.(*ecdsa.PrivateKey)
		if !ok {
			return nil, problem("identity", nil)
		}
	}
	der, err := x509.CreateCertificate(random, t, issuer, key.Public(), signer)
	if err != nil {
		clear(keyPEM)
		return nil, err
	}
	data := map[string][]byte{"tls.crt": pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), "tls.key": keyPEM}
	if spec.parent != nil {
		data["ca.crt"] = append([]byte(nil), spec.parent.Data["tls.crt"]...)
	}
	return data, nil
}
func certificateKey(random io.Reader) ([]byte, *ecdsa.PrivateKey, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), random)
	if err != nil {
		return nil, nil, err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	defer clear(der)
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), key, nil
}

func sshKey(random io.Reader) (map[string][]byte, error) {
	raw, key, err := keyPair(random)
	if err != nil {
		return nil, err
	}
	defer clear(key)
	pub, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		clear(raw)
		return nil, err
	}
	return map[string][]byte{"key": raw, "public": ssh.MarshalAuthorizedKey(pub)}, nil
}
func (e *Engine) identities(ctx context.Context, p *Plan) (Identities, error) {
	var i Identities
	ns := p.choices.Namespace
	for _, item := range []struct {
		name string
		dst  **corev1.Secret
		make func() (map[string][]byte, error)
	}{
		{"oberth-ca", &i.CA, func() (map[string][]byte, error) {
			return certificate(e.Random, e.Now(), certificateSpec{ca: true, host: "Oberth"})
		}},
		{"oberth-ssh-host", &i.Host, func() (map[string][]byte, error) { return sshKey(e.Random) }},
		{"oberth-forge-key", &i.Forge, func() (map[string][]byte, error) { return sshKey(e.Random) }},
		{"oberth-store-tls", &i.Store, func() (map[string][]byte, error) {
			return certificate(e.Random, e.Now(), certificateSpec{host: storeDNS(ns), parent: i.CA})
		}},
	} {
		s, err := e.plannedSecret(ctx, ns, item.name, item.make)
		if err != nil {
			i.clear()
			return Identities{}, err
		}
		*item.dst = s
	}
	if err := e.verifyBundle(i, ns); err != nil {
		i.clear()
		return Identities{}, err
	}
	return i, nil
}
func (e *Engine) plannedSecret(ctx context.Context, ns, name string, makeData func() (map[string][]byte, error)) (*corev1.Secret, error) {
	s, err := e.Cluster.Client.CoreV1().Secrets(ns).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		data, err := makeData()
		if err != nil {
			return nil, problem("identity", err)
		}
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns}, Data: data}, nil
	}
	if err != nil || !validSecret(s) {
		return nil, problem("identity", err)
	}
	return s, nil
}
func validSecret(s *corev1.Secret) bool {
	if s == nil {
		return false
	}
	if key := s.Data["key"]; len(key) > 0 {
		signer, err := ssh.ParsePrivateKey(key)
		return err == nil && bytes.Equal(ssh.MarshalAuthorizedKey(signer.PublicKey()), s.Data["public"])
	}
	cert, err := clientcert.ParseCertificate(s.Data["tls.crt"])
	if err != nil {
		return false
	}
	pair, err := tls.X509KeyPair(s.Data["tls.crt"], s.Data["tls.key"])
	if err != nil {
		return false
	}
	key, ok := pair.PrivateKey.(*ecdsa.PrivateKey)
	return ok && key.Curve == elliptic.P256() && cert.SignatureAlgorithm == x509.ECDSAWithSHA256 &&
		(s.Name != "oberth-ca" || cert.IsCA && cert.KeyUsage&x509.KeyUsageCertSign != 0)
}

func (e *Engine) verifyBundle(i Identities, namespace string) error {
	ca, err := clientcert.ParseCertificate(i.CA.Data["tls.crt"])
	if err != nil || ca.CheckSignatureFrom(ca) != nil {
		return problem("identity", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	leaf, err := clientcert.ParseCertificate(i.Store.Data["tls.crt"])
	if err != nil {
		return problem("identity", err)
	}
	_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: storeDNS(namespace), CurrentTime: e.Now()})
	if err != nil {
		return problem("identity", err)
	}
	return nil
}

func (i Identities) hostFingerprint() string {
	key, _, _, _, err := ssh.ParseAuthorizedKey(i.Host.Data["public"])
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}
