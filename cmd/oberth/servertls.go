package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

// servedCertificate is the HTTPS identity every listener serves. A
// PKI-issued leaf is swapped in place when renewed, so renewal needs neither
// a restart nor a client change (clients pin the CA).
type servedCertificate struct {
	current atomic.Pointer[tls.Certificate]
}

func loadServedCertificate(certPath, keyPath string) (*servedCertificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	served := &servedCertificate{}
	served.current.Store(&pair)
	return served, nil
}

func (served *servedCertificate) getCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return served.current.Load(), nil
}

// serverPKI configures leaf issuance from OpenBao's PKI engine. The chart
// sets these variables only when the installer provisioned the CA.
type serverPKI struct {
	request secretstore.ServerCertificateRequest
}

func serverPKIFromEnvironment(namespace string) (serverPKI, bool) {
	mount := strings.TrimSpace(os.Getenv("OBERTH_SERVER_PKI_MOUNT"))
	role := strings.TrimSpace(os.Getenv("OBERTH_SERVER_PKI_ROLE"))
	if mount == "" || role == "" {
		return serverPKI{}, false
	}
	dnsNames, ips := secretstore.ServerTLSNames(namespace, strings.Split(os.Getenv("OBERTH_TLS_NAMES"), ","))
	return serverPKI{request: secretstore.ServerCertificateRequest{
		Mount: mount, Role: role, DNSNames: dnsNames, IPs: ips,
		TTL: strings.TrimSpace(os.Getenv("OBERTH_SERVER_PKI_TTL")),
	}}, true
}

// issueServerLeaf requests a fresh leaf and returns it as identity files:
// tls.crt, tls.key and ca.crt.
func (pki serverPKI) issueServerLeaf(ctx context.Context, store *baoIdentityStore) (map[string][]byte, secretstore.IssuedServerCertificate, error) {
	issued, err := store.client.IssueServerCertificate(ctx, pki.request)
	if err != nil {
		return nil, issued, fmt.Errorf("issue server certificate from %s/%s: %w", pki.request.Mount, pki.request.Role, err)
	}
	return map[string][]byte{"tls.crt": issued.Certificate, "tls.key": issued.Key, "ca.crt": issued.CA}, issued, nil
}

// renewServerLeaf keeps the served leaf current: it re-issues at two thirds
// of the lifetime, rewrites the identity files and swaps the served
// certificate. Failures are retried every minute until the leaf expires.
func renewServerLeaf(ctx context.Context, pki serverPKI, store *baoIdentityStore, served *servedCertificate, current secretstore.IssuedServerCertificate, logger *log.Logger) {
	renewAt := current.RenewAt()
	for {
		wait := time.Until(renewAt)
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		files, issued, err := pki.issueServerLeaf(ctx, store)
		if err == nil {
			err = replaceServedLeaf(files, served)
		}
		for _, value := range files {
			clear(value)
		}
		if err != nil {
			logger.Printf("server certificate renewal failed (current leaf expires %s); retrying in 1m: %v", current.NotAfter.UTC().Format(time.RFC3339), err)
			renewAt = time.Now().Add(time.Minute)
			continue
		}
		current = issued
		renewAt = current.RenewAt()
		logger.Printf("server certificate renewed; valid until %s", current.NotAfter.UTC().Format(time.RFC3339))
	}
}

func replaceServedLeaf(files map[string][]byte, served *servedCertificate) error {
	pair, err := tls.X509KeyPair(files["tls.crt"], files["tls.key"])
	if err != nil {
		return err
	}
	if err := materializeIdentity("server", files); err != nil {
		return err
	}
	served.current.Store(&pair)
	return nil
}

// serverLeafRenewal carries a PKI-issued leaf from identity bootstrap to the
// listener setup that renews it.
type serverLeafRenewal struct {
	pki    serverPKI
	store  *baoIdentityStore
	issued secretstore.IssuedServerCertificate
}
