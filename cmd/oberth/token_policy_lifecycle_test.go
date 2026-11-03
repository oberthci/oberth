package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"database/sql"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

type tokenWarningBuffer struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (buffer *tokenWarningBuffer) Write(body []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.Write(body)
}

func (buffer *tokenWarningBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.buffer.String()
}

func TestCLIUsesLiveTokenPolicyAndRejectsAboveServerMaximum(t *testing.T) {
	key, private := newTestUplinkKey(t)
	directory := t.TempDir()
	certificate := filepath.Join(directory, "tls.crt")
	database := filepath.Join(directory, "oberth.sqlite")
	writeTestCertificate(t, certificate, private.Public().(ed25519.PublicKey), private)
	policy := model.UplinkTokenPolicy{DefaultTTL: 30 * 24 * time.Hour, MaxTTL: 60 * 24 * time.Hour}
	socket := privateAdminSocket(t)
	server := newAdminGateServerWithTokenPolicy(func(context.Context) error { return nil }, database, nil, policy)
	listener, err := listenAdminSocket(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	defer server.Close()
	go func() { _ = server.Serve(listener) }()
	deps := uplinkDependencies{mutationGate: func(ctx context.Context, operation, path string) error {
		return requestAdminMutationGate(ctx, socket, operation, path)
	}, tokenPolicy: func(ctx context.Context, path string) (model.UplinkTokenPolicy, error) {
		return requestUplinkTokenPolicy(ctx, socket, path)
	}}
	args := []string{"add", "--database", database, "--tls-cert", certificate, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "policy@host"}
	if err := runUplinkWithDependencies(t.Context(), args, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	db, err := store.OpenAdminClient(t.Context(), database, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	statuses, err := db.ListUplinkStatuses(t.Context())
	if err != nil || len(statuses) != 1 || statuses[0].ExpiresAt == nil {
		t.Fatalf("missing default expiry: %+v %v", statuses, err)
	}
	if remaining := time.Until(*statuses[0].ExpiresAt); remaining < policy.DefaultTTL-time.Minute || remaining > policy.DefaultTTL {
		t.Fatalf("CLI ignored live default: %s", remaining)
	}
	var refused bytes.Buffer
	if err := runUplinkWithDependencies(t.Context(), append([]string{"add", "--ttl", "1464h"}, args[1:]...), strings.NewReader(""), &refused, deps); err == nil {
		t.Fatal("CLI accepted TTL above server maximum")
	}
	if strings.Contains(refused.String(), "shown once") {
		t.Fatal("printed credential header before rejecting TTL")
	}
	if _, err := requestUplinkTokenPolicy(t.Context(), socket, "other-database"); err == nil {
		t.Fatal("policy accepted for a different database")
	}
	// Simulate an existing, pre-policy token without changing its other state.
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=NULL`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	var listing bytes.Buffer
	if err := runUplinkList(t.Context(), []string{"--database", database}, &listing); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(listing.String(), "NO EXPIRY (rotate)") {
		t.Fatalf("legacy token not flagged: %s", &listing)
	}
}

func TestCLITokenPolicyFailureDoesNotFallBackToDefaults(t *testing.T) {
	key, private := newTestUplinkKey(t)
	directory := t.TempDir()
	certificate := filepath.Join(directory, "tls.crt")
	database := filepath.Join(directory, "oberth.sqlite")
	writeTestCertificate(t, certificate, private.Public().(ed25519.PublicKey), private)
	err := runUplinkWithDependencies(t.Context(), []string{"add", "--database", database, "--tls-cert", certificate, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key))), "policy@host"}, strings.NewReader(""), io.Discard, uplinkDependencies{mutationGate: allowTestMutation, tokenPolicy: func(context.Context, string) (model.UplinkTokenPolicy, error) {
		return model.UplinkTokenPolicy{}, errors.New("policy unavailable")
	}})
	if err == nil {
		t.Fatal("issuance fell back without live policy")
	}
	if _, err := os.Stat(database); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("database opened without policy: %v", err)
	}
}

func TestLegacyTokenWarningRepeatsHourlyAndStopsAfterRotation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output tokenWarningBuffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		var count atomic.Int64
		count.Store(2)
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchNonExpiringUplinkTokens(ctx, func(context.Context) (int, error) { return int(count.Load()), nil }, log.New(&output, "", 0))
		}()
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 1 {
			t.Fatalf("startup warning: %s", &output)
		}
		time.Sleep(time.Hour)
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 2 {
			t.Fatalf("missing periodic warning: %s", &output)
		}
		count.Store(0)
		time.Sleep(time.Hour)
		synctest.Wait()
		if strings.Count(output.String(), "have no expiry") != 2 {
			t.Fatalf("warning persisted after rotation: %s", &output)
		}
		cancel()
		<-done
	})
}

func TestLegacyTokenWarningReadErrorDoesNotExposeDetails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var output tokenWarningBuffer
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan struct{})
		go func() {
			defer close(done)
			watchNonExpiringUplinkTokens(ctx, func(context.Context) (int, error) { return 0, errors.New("private diagnostic material") }, log.New(&output, "", 0))
		}()
		synctest.Wait()
		if !strings.Contains(output.String(), "unable to check") || strings.Contains(output.String(), "private diagnostic material") {
			t.Fatalf("unsafe or missing warning: %s", &output)
		}
		cancel()
		<-done
	})
}

func TestUplinkRotateIgnoresRevokedExpiredCredentials(t *testing.T) {
	directory := t.TempDir()
	database := filepath.Join(directory, "oberth.sqlite")
	oldKey, certificateKey := newTestUplinkKey(t)
	certificatePath := filepath.Join(directory, "tls.crt")
	writeTestCertificate(t, certificatePath, certificateKey.Public().(ed25519.PublicKey), certificateKey)
	deps := uplinkDependencies{mutationGate: allowTestMutation}
	if err := runUplinkWithDependencies(context.Background(), []string{
		"add", "--database", database, "--tls-cert", certificatePath,
		strings.TrimSpace(string(ssh.MarshalAuthorizedKey(oldKey))), "agent@host",
	}, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	revokedKey, _ := newTestUplinkKey(t)
	if err := runUplinkWithDependencies(t.Context(), []string{"add", "--database", database, "--tls-cert", certificatePath, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(revokedKey))), "agent@host"}, strings.NewReader(""), io.Discard, deps); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", database)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=?, revoked_at=? WHERE id IN (SELECT token_credential_id FROM uplinks WHERE fingerprint=?)`, time.Now().Add(-time.Hour).UnixNano(), time.Now().UnixNano(), ssh.FingerprintSHA256(revokedKey))
	raw.Close()
	if err != nil {
		t.Fatal(err)
	}
	newKey, _ := newTestUplinkKey(t)
	var output bytes.Buffer
	if err := runUplinkWithDependencies(context.Background(), []string{
		"rotate", "--database", database, "--tls-cert", certificatePath, "-", "agent@host",
	}, bytes.NewReader(ssh.MarshalAuthorizedKey(newKey)), &output, deps); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "Uplink token for agent@host") ||
		!strings.Contains(output.String(), "removed uplink agent@host ("+ssh.FingerprintSHA256(oldKey)+")") {
		t.Fatalf("rotate output = %q", output.String())
	}
}
