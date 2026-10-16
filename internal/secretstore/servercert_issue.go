package secretstore

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// ServerCertificateRequest asks OpenBao's PKI engine for a server leaf.
type ServerCertificateRequest struct {
	// Mount is the PKI secrets engine mount, e.g. "oberth-pki".
	Mount string
	// Role is the PKI role that limits names, key type and lifetime.
	Role     string
	DNSNames []string
	IPs      []string
	// TTL is the requested lifetime, e.g. "720h"; empty takes the role's.
	TTL string
}

// IssuedServerCertificate is a PKI-issued server leaf. Key is private and
// must be cleared by the caller once materialized.
type IssuedServerCertificate struct {
	Certificate []byte // PEM leaf
	Key         []byte // PEM private key
	CA          []byte // PEM issuing CA, the trust anchor clients pin
	NotBefore   time.Time
	NotAfter    time.Time
}

var pkiSegment = identitySegment

// IssueServerCertificate issues a short-lived server certificate from the
// installer-managed CA. The CA key never leaves OpenBao; the server holds
// only its current leaf, and clients pin the CA, so renewing the leaf needs
// no client change.
func (client *Client) IssueServerCertificate(ctx context.Context, request ServerCertificateRequest) (IssuedServerCertificate, error) {
	if !pkiSegment.MatchString(request.Mount) || !pkiSegment.MatchString(request.Role) {
		return IssuedServerCertificate{}, errors.New("invalid server PKI mount or role")
	}
	if len(request.DNSNames) == 0 {
		return IssuedServerCertificate{}, errors.New("server certificate needs at least one DNS name")
	}
	if err := client.requireVerifiedHTTPS("server certificate issuance"); err != nil {
		return IssuedServerCertificate{}, err
	}
	session, err := client.login(ctx)
	if err != nil {
		return IssuedServerCertificate{}, err
	}
	defer client.logout(ctx, session)
	data := map[string]any{
		"common_name":          request.DNSNames[0],
		"alt_names":            strings.Join(request.DNSNames[1:], ","),
		"ip_sans":              strings.Join(request.IPs, ","),
		"exclude_cn_from_sans": false,
		"format":               "pem",
		"private_key_format":   "pkcs8",
	}
	if request.TTL != "" {
		data["ttl"] = request.TTL
	}
	response, err := session.Logical().WriteWithContext(ctx, request.Mount+"/issue/"+request.Role, data)
	if err != nil {
		return IssuedServerCertificate{}, sanitizeSecretStoreHTTPError("server certificate issuance", err)
	}
	if response == nil || response.Data == nil {
		return IssuedServerCertificate{}, errors.New("server certificate issuance returned no data")
	}
	field := func(name string) ([]byte, error) {
		value, ok := response.Data[name].(string)
		if !ok || strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("server certificate issuance returned no %s", name)
		}
		return []byte(strings.TrimSpace(value) + "\n"), nil
	}
	issued := IssuedServerCertificate{}
	if issued.Certificate, err = field("certificate"); err != nil {
		return IssuedServerCertificate{}, err
	}
	if issued.Key, err = field("private_key"); err != nil {
		return IssuedServerCertificate{}, err
	}
	if issued.CA, err = field("issuing_ca"); err != nil {
		clear(issued.Key)
		return IssuedServerCertificate{}, err
	}
	if err := issued.validate(request); err != nil {
		clear(issued.Key)
		return IssuedServerCertificate{}, err
	}
	return issued, nil
}

// validate checks that the leaf matches its key, chains to the returned CA,
// covers every requested name and records its validity window.
func (issued *IssuedServerCertificate) validate(request ServerCertificateRequest) error {
	pair, err := tls.X509KeyPair(issued.Certificate, issued.Key)
	if err != nil {
		return fmt.Errorf("issued server certificate does not match its key: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse issued server certificate: %w", err)
	}
	roots := x509.NewCertPool()
	block, _ := pem.Decode(issued.CA)
	if block == nil {
		return errors.New("issuing CA is not PEM")
	}
	ca, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return fmt.Errorf("parse issuing CA: %w", err)
	}
	roots.AddCert(ca)
	for _, name := range append(append([]string(nil), request.DNSNames...), request.IPs...) {
		if _, err := leaf.Verify(x509.VerifyOptions{DNSName: name, Roots: roots}); err != nil {
			return fmt.Errorf("issued server certificate does not verify for %s: %w", name, err)
		}
	}
	issued.NotBefore, issued.NotAfter = leaf.NotBefore, leaf.NotAfter
	return nil
}

// RenewAt is when a leaf should be replaced: two thirds into its lifetime,
// leaving a third for retries if OpenBao is briefly sealed or unreachable.
func (issued IssuedServerCertificate) RenewAt() time.Time {
	lifetime := issued.NotAfter.Sub(issued.NotBefore)
	return issued.NotBefore.Add(lifetime * 2 / 3)
}

// ServerTLSNames are the names the server certificate covers: the in-cluster
// service names, the goproxy service names, and operator-supplied extras.
func ServerTLSNames(namespace string, extra []string) (dnsNames, ips []string) {
	names := []string{
		"oberth", "oberth." + namespace, "oberth." + namespace + ".svc", "oberth." + namespace + ".svc.cluster.local",
		"oberth-goproxy", "oberth-goproxy." + namespace, "oberth-goproxy." + namespace + ".svc", "oberth-goproxy." + namespace + ".svc.cluster.local",
	}
	seen := map[string]bool{}
	for _, name := range append(names, extra...) {
		name = strings.TrimSpace(name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		if net.ParseIP(name) != nil {
			ips = append(ips, name)
		} else {
			dnsNames = append(dnsNames, name)
		}
	}
	return dnsNames, ips
}
