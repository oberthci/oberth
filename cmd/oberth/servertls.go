package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/oberthci/oberth/internal/secretstore"
)

// servedCertificate is the HTTPS identity every listener serves. A
// PKI-issued leaf is swapped in place when renewed, so renewal needs neither
// a restart nor a client change (clients pin the CA).
type servedCertificate struct {
	current      atomic.Pointer[tls.Certificate]
	renewalError atomic.Pointer[string]
}

func loadServedCertificate(certPath, keyPath string) (*servedCertificate, error) {
	pair, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, err
	}
	if pair.Leaf == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return nil, err
		}
	}
	// The CA is the identity file next to the leaf, when there is one.
	if authority, readErr := os.ReadFile(filepath.Join(filepath.Dir(certPath), "ca.crt")); readErr == nil { //nolint:gosec // G304: the server's own identity directory.
		presentAuthority(&pair, authority)
	}
	served := &servedCertificate{}
	served.current.Store(&pair)
	return served, nil
}

// presentAuthority appends the issuing CA to the chain the server presents.
// A client that pins the CA by its SHA-256 (the plugin's /oberth:connect)
// reads it from the handshake and compares it with the pin before trusting
// anything; a verifying client ignores it. Only a certificate that issued
// the last one of the chain is added, and never one already in it.
func presentAuthority(pair *tls.Certificate, authorityPEM []byte) {
	for {
		var block *pem.Block
		block, authorityPEM = pem.Decode(authorityPEM)
		if block == nil {
			return
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		authority, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !authority.IsCA {
			continue
		}
		last, err := x509.ParseCertificate(pair.Certificate[len(pair.Certificate)-1])
		if err != nil || last.Equal(authority) || last.CheckSignatureFrom(authority) != nil {
			continue
		}
		pair.Certificate = append(pair.Certificate, block.Bytes)
	}
}

func (served *servedCertificate) readyAt(now time.Time) error {
	current := served.current.Load()
	if current == nil || current.Leaf == nil {
		return errors.New("server TLS leaf is unavailable")
	}
	leaf := current.Leaf
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return fmt.Errorf("server TLS leaf is outside its validity window (expires %s)", leaf.NotAfter.UTC().Format(time.RFC3339))
	}
	margin := min(leaf.NotAfter.Sub(leaf.NotBefore)/10, time.Hour)
	if failure := served.renewalError.Load(); failure != nil && !now.Add(margin).Before(leaf.NotAfter) {
		return fmt.Errorf("server TLS leaf expires %s and renewal failed: %s", leaf.NotAfter.UTC().Format(time.RFC3339), *failure)
	}
	return nil
}

func (served *servedCertificate) writeMetrics(w io.Writer) {
	if current := served.current.Load(); current != nil && current.Leaf != nil {
		_, _ = fmt.Fprintf(w, "# TYPE oberth_server_tls_leaf_not_after_seconds gauge\noberth_server_tls_leaf_not_after_seconds %d\n", current.Leaf.NotAfter.Unix())
	}
	failure := served.renewalError.Load()
	value := 0
	if failure != nil {
		value = 1
	}
	_, _ = fmt.Fprintf(w, "# TYPE oberth_server_tls_renewal_error gauge\noberth_server_tls_renewal_error %d\n", value)
	if failure != nil {
		// A single bounded diagnostic series; no tokens, private key or bodies
		// from non-PKI endpoints enter the issuer's sanitized errors.
		detail := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, *failure)
		runes := []rune(detail)
		if len(runes) > 1024 {
			detail = string(runes[:1024])
		}
		_, _ = fmt.Fprintf(w, "# TYPE oberth_server_tls_last_renewal_error_info gauge\noberth_server_tls_last_renewal_error_info{reason=%q} 1\n", detail)
	}
}

// startServerTask ties both cancellation and completion to the server scope,
// including early returns before its parent context has been canceled.
func startServerTask(parent context.Context, run func(context.Context)) func() {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() { defer close(done); run(ctx) }()
	return func() { cancel(); <-done }
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
		issued, err := renewServedLeaf(ctx, served, func(ctx context.Context) (map[string][]byte, secretstore.IssuedServerCertificate, error) {
			return pki.issueServerLeaf(ctx, store)
		}, func(files map[string][]byte) error { return replaceServedLeaf(files, served) })
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

func renewServedLeaf(ctx context.Context, served *servedCertificate, issue func(context.Context) (map[string][]byte, secretstore.IssuedServerCertificate, error), replace func(map[string][]byte) error) (secretstore.IssuedServerCertificate, error) {
	files, issued, err := issue(ctx)
	defer func() {
		for _, value := range files {
			clear(value)
		}
	}()
	if err == nil {
		err = replace(files)
	}
	if err != nil {
		detail := err.Error()
		served.renewalError.Store(&detail)
	} else {
		served.renewalError.Store(nil)
	}
	return issued, err
}

func replaceServedLeaf(files map[string][]byte, served *servedCertificate) error {
	pair, err := tls.X509KeyPair(files["tls.crt"], files["tls.key"])
	if err != nil {
		return err
	}
	if pair.Leaf == nil {
		pair.Leaf, err = x509.ParseCertificate(pair.Certificate[0])
		if err != nil {
			return err
		}
	}
	presentAuthority(&pair, files["ca.crt"])
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
