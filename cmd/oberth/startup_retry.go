package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

// retryStartup retries only classified temporary failures and bounds the whole
// operation, including in-flight network calls and waits.
func retryStartup(ctx context.Context, budget, delay time.Duration, attempt func(context.Context) error, transient func(error) bool, logger *log.Logger) error {
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	var last error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(err, last)
		}
		last = attempt(ctx)
		if last == nil {
			return nil
		}
		if !transient(last) {
			return last
		}
		if logger != nil {
			logger.Printf("WARN: startup dependency unavailable; retrying in %s: %v", delay, last)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return errors.Join(ctx.Err(), last)
		case <-timer.C:
		}
		delay = min(delay*2, 10*time.Second)
	}
}
func transientIdentityStartup(err error) bool {
	return errors.Is(err, os.ErrNotExist) || secretstore.IsUnavailable(err)
}

// Kubernetes HTTPS probes do not authenticate the serving certificate. During
// bootstrap this short-lived, in-memory identity serves health endpoints only;
// no application or authenticated route is available until the real identity
// and owner recovery have been installed. Close and join before binding the
// normal listener so this certificate never enters the application server.
func startBootstrapHealth(ctx context.Context, address string) (func(), string, error) {
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		clear(key)
		return nil, "", err
	}
	now := time.Now()
	certificate := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "oberth bootstrap probes"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(10 * time.Minute), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, public, key)
	if err != nil {
		clear(key)
		return nil, "", err
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp", address)
	if err != nil {
		clear(key)
		return nil, "", err
	}
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if time.Now().Before(now.Add(5*time.Minute)) && r.Method == http.MethodGet && (r.URL.Path == "/livez" || r.URL.Path == "/healthz") {
			_, _ = io.WriteString(w, "starting\n")
			return
		}
		http.Error(w, "startup incomplete", http.StatusServiceUnavailable)
	})
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 5 * time.Second, MaxHeaderBytes: 8 << 10, ErrorLog: log.New(io.Discard, "", 0), TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}}
	done := make(chan struct{})
	go func() { defer close(done); _ = server.Serve(tls.NewListener(listener, server.TLSConfig)) }()
	var once sync.Once
	stop := func() { once.Do(func() { _ = server.Close(); <-done }) }
	stopOnCancel := context.AfterFunc(ctx, stop)
	return func() { stopOnCancel(); stop() }, listener.Addr().String(), nil
}

func inClusterConfigWithRetry(ctx context.Context, load func() error) error {
	const token = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- Standard projection path, not a credential.
	for attempt := 0; attempt < 3; attempt++ {
		err := load()
		if err == nil {
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) || attempt == 2 {
			return fmt.Errorf("load in-cluster Kubernetes config (ServiceAccount token %s): %w", token, err)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}
