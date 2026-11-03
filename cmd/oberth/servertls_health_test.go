package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/secretstore"
)

func TestServerLeafRenewalControlsReadinessBeforeExpiry(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	served := &servedCertificate{}
	served.current.Store(&tls.Certificate{Leaf: &x509.Certificate{NotBefore: now.Add(-9 * time.Hour), NotAfter: now.Add(time.Hour)}})
	refusal := errors.New("name ci.example.com not allowed by role oberth-server")
	failing := func(context.Context) (map[string][]byte, secretstore.IssuedServerCertificate, error) {
		return nil, secretstore.IssuedServerCertificate{}, refusal
	}
	_, err := renewServedLeaf(t.Context(), served, failing, func(map[string][]byte) error { t.Fatal("failed issuance replaced served leaf"); return nil })
	if !errors.Is(err, refusal) {
		t.Fatalf("renewal lost cause: %v", err)
	}
	if err = served.readyAt(now.Add(-time.Minute)); err != nil {
		t.Fatalf("failed renewal with sufficient remaining lifetime: %v", err)
	}
	if err = served.readyAt(now); err == nil || !strings.Contains(err.Error(), refusal.Error()) {
		t.Fatalf("near-expiry leaf must fail readiness with issuance reason: %v", err)
	}
	var metrics bytes.Buffer
	served.writeMetrics(&metrics)
	if !strings.Contains(metrics.String(), "oberth_server_tls_leaf_not_after_seconds") || !strings.Contains(metrics.String(), "oberth_server_tls_renewal_error 1") || !strings.Contains(metrics.String(), "not allowed by role") {
		t.Fatalf("missing renewal metrics: %s", &metrics)
	}
	issued := secretstore.IssuedServerCertificate{NotBefore: now, NotAfter: now.Add(24 * time.Hour)}
	key := []byte("ephemeral test key")
	_, err = renewServedLeaf(t.Context(), served, func(context.Context) (map[string][]byte, secretstore.IssuedServerCertificate, error) {
		return map[string][]byte{"tls.key": key}, issued, nil
	}, func(map[string][]byte) error {
		served.current.Store(&tls.Certificate{Leaf: &x509.Certificate{NotBefore: issued.NotBefore, NotAfter: issued.NotAfter}})
		return nil
	})
	if err != nil || served.readyAt(now) != nil {
		t.Fatalf("successful renewal did not restore readiness: %v", err)
	}
	if !bytes.Equal(key, make([]byte, len(key))) {
		t.Fatal("issued private bytes were not cleared")
	}
	metrics.Reset()
	served.writeMetrics(&metrics)
	if !strings.Contains(metrics.String(), "oberth_server_tls_renewal_error 0") || strings.Contains(metrics.String(), refusal.Error()) {
		t.Fatalf("successful renewal kept stale error: %s", &metrics)
	}
	if served.readyAt(issued.NotAfter) == nil {
		t.Fatal("expired leaf remained ready without a recorded renewal failure")
	}
}

func TestServerBackgroundTaskShutdownJoinsWorker(t *testing.T) {
	canceled, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	stop := startServerTask(t.Context(), func(ctx context.Context) { <-ctx.Done(); close(canceled); <-release })
	go func() { stop(); close(done) }()
	<-canceled
	select {
	case <-done:
		t.Fatal("shutdown returned before background worker exited")
	default:
	}
	close(release)
	<-done
}
