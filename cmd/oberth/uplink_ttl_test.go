package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/store"

	"golang.org/x/crypto/ssh"
)

func TestUplinkAddTTLAndExpiryListing(t *testing.T) {
	key, private := newTestUplinkKey(t)
	directory := t.TempDir()
	certificate := filepath.Join(directory, "tls.crt")
	database := filepath.Join(directory, "oberth.sqlite")
	writeTestCertificate(t, certificate, private.Public().(ed25519.PublicKey), private)
	deps := uplinkDependencies{mutationGate: allowTestMutation}
	if err := runUplinkWithDependencies(t.Context(), []string{"add", "--ttl", "1h", "--database", database, "--tls-cert", certificate,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "expiry@host"}, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := runUplinkList(t.Context(), []string{"--database", database}, &output); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenAdminClient(t.Context(), database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statuses, err := db.ListUplinkStatuses(t.Context())
	if err != nil || len(statuses) != 1 || statuses[0].ExpiresAt == nil {
		t.Fatalf("expiry missing from storage: %+v, %v", statuses, err)
	}
	if delta := time.Until(*statuses[0].ExpiresAt); delta < 59*time.Minute || delta > time.Hour {
		t.Fatalf("ttl not respected: %s", delta)
	}
	if !strings.Contains(output.String(), "EXPIRES") || !strings.Contains(output.String(), statuses[0].ExpiresAt.UTC().Format(time.RFC3339)) {
		t.Fatalf("missing expiry listing: %s", &output)
	}
	expires := *statuses[0].ExpiresAt
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	nextKey, _ := newTestUplinkKey(t)
	if err := runUplinkRotate(t.Context(), []string{"--database", database, "--tls-cert", certificate,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(nextKey))), "expiry@host"}, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	db, err = store.OpenAdminClient(t.Context(), database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statuses, err = db.ListUplinkStatuses(t.Context())
	if err != nil || len(statuses) != 1 || statuses[0].ExpiresAt == nil || !statuses[0].ExpiresAt.Equal(expires) {
		t.Fatalf("rotation changed expiry: %+v %v", statuses, err)
	}
}

func TestUplinkAddRejectsInvalidTTLBeforeGate(t *testing.T) {
	for _, ttl := range []string{"0", "-1h", "forever"} {
		called := false
		err := runUplinkWithDependencies(t.Context(), []string{"add", "--ttl", ttl, "-", "expiry@host"}, strings.NewReader(""), io.Discard,
			uplinkDependencies{mutationGate: func(_ context.Context, _, _ string) error { called = true; return nil }})
		if err == nil || called {
			t.Fatalf("invalid ttl %q: err=%v gate=%v", ttl, err, called)
		}
	}
}
