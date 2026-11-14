package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/auth"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

func firstAdminLiveCSR(t *testing.T) string {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func firstAdminLiveConfig(t *testing.T) (firstAdminConfig, *store.Store, *[]string, *sync.Mutex) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "oberth.sqlite")
	daemon, err := store.CreateGenesis(t.Context(), path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = daemon.Close() })
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(files["ca.key"])
	authority, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	socket := privateAdminSocket(t)
	listener, err := listenAdminSocket(t.Context(), socket)
	if err != nil {
		t.Fatal(err)
	}
	gate := newAdminGateServer(func(ctx context.Context) error {
		_, _, err := daemon.VerifyLocalAuditMutationState(ctx, model.AuditHead{}, func(model.AuditHead, model.AuditAnchor) error { return nil })
		return err
	}, path, nil)
	var calls []string
	var lock sync.Mutex
	handler := gate.Handler
	gate.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/mutation-gate" {
			lock.Lock()
			calls = append(calls, r.Header.Get("X-Oberth-Admin-Operation"))
			lock.Unlock()
		}
		handler.ServeHTTP(w, r)
	})
	done := make(chan error, 1)
	go func() { done <- gate.Serve(listener) }()
	t.Cleanup(func() {
		_ = gate.Close()
		if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
			t.Error(err)
		}
	})
	return firstAdminConfig{DatabasePath: path, SocketPath: socket, Authority: authority, ServerCA: string(files["ca.crt"]), ServerCertificate: string(files["ca.crt"])}, daemon, &calls, &lock
}

func invokeFirstAdminLive(t *testing.T, config firstAdminConfig, request api.FirstAdminRequest) (api.FirstAdminResult, error) {
	t.Helper()
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	defer func() { clear(output.Bytes()) }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	err = runCreateFirstAdmin(ctx, bytes.NewReader(body), &output, config)
	if err != nil {
		if output.Len() != 0 {
			t.Fatal("failed enrollment exposed output")
		}
		return api.FirstAdminResult{}, err
	}
	var result api.FirstAdminResult
	if json.Unmarshal(output.Bytes(), &result) != nil || result.Token == "" || result.InstallerCertificate == "" {
		t.Fatal("incomplete enrollment response")
	}
	return result, nil
}

func requireFirstAdminLiveCredential(t *testing.T, config firstAdminConfig, daemon *store.Store, result api.FirstAdminResult) int64 {
	t.Helper()
	actor, err := (auth.Authenticator{Tokens: daemon}).Authenticate(t.Context(), result.Token)
	if err != nil || !actor.Admin || len(actor.Scope) != 0 {
		t.Fatal("returned bearer does not authenticate as unscoped admin", err)
	}
	block, _ := pem.Decode([]byte(result.InstallerCertificate))
	if block == nil {
		t.Fatal("invalid installer certificate PEM")
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(config.Authority.Certificate)
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatal("installer certificate is not signed for client use", err)
	}
	uplink, err := daemon.UplinkByClientPin(t.Context(), clientcert.Pin(leaf))
	if err != nil || uplink.ID != actor.ID || !uplink.Admin {
		t.Fatal("returned installer certificate is not bound to its admin", err)
	}
	return uplink.ID
}

func TestRunCreateFirstAdminLiveGate(t *testing.T) {
	config, daemon, calls, lock := firstAdminLiveConfig(t)
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	sshPublic, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	request := api.FirstAdminRequest{Identity: "operator@host", SSHPublicKey: string(ssh.MarshalAuthorizedKey(sshPublic)), InstallerCSR: firstAdminLiveCSR(t)}
	var first, second api.FirstAdminResult
	var id int64
	if !t.Run("FirstRun", func(t *testing.T) {
		first, err = invokeFirstAdminLive(t, config, request)
		if err != nil {
			t.Fatal("first enrollment failed against the live gate", err)
		}
		id = requireFirstAdminLiveCredential(t, config, daemon, first)
	}) {
		return
	}
	if !t.Run("SameKeyRedelivers", func(t *testing.T) {
		second, err = invokeFirstAdminLive(t, config, request)
		if err != nil {
			t.Fatal("same-key redelivery failed", err)
		}
		if next := requireFirstAdminLiveCredential(t, config, daemon, second); next != id || first.Token == second.Token {
			t.Fatal("retry did not reissue credentials for the same uplink")
		}
		if _, err := (auth.Authenticator{Tokens: daemon}).Authenticate(t.Context(), first.Token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatal("redelivery retained the displaced bearer", err)
		}
	}) {
		return
	}
	different := request
	different.InstallerCSR = firstAdminLiveCSR(t)
	if !t.Run("DifferentKeyRefusedWhileActive", func(t *testing.T) {
		if _, err := invokeFirstAdminLive(t, config, different); !errors.Is(err, store.ErrInvalidState) {
			t.Fatal("different key replaced an active admin", err)
		}
		if current := requireFirstAdminLiveCredential(t, config, daemon, second); current != id {
			t.Fatal("refusal changed the active admin")
		}
	}) {
		return
	}
	if !t.Run("RecoveryWithoutActiveAdmin", func(t *testing.T) {
		// Advance persisted expiry boundaries past creation without waiting an hour.
		db, err := sql.Open("sqlite", config.DatabasePath)
		if err != nil {
			t.Fatal(err)
		}
		_, err = db.ExecContext(t.Context(), `UPDATE token_credentials SET expires_at=created_at+1; UPDATE uplink_client_pins SET expires_at=created_at+1`)
		closeErr := db.Close()
		if err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		if _, err := (auth.Authenticator{Tokens: daemon}).Authenticate(t.Context(), second.Token); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatal("fixture admin did not expire", err)
		}
		recovered, err := invokeFirstAdminLive(t, config, different)
		if err != nil {
			t.Fatal("inactive sole admin could not recover", err)
		}
		if current := requireFirstAdminLiveCredential(t, config, daemon, recovered); current != id {
			t.Fatal("recovery duplicated the admin")
		}
	}) {
		return
	}
	lock.Lock()
	defer lock.Unlock()
	for _, operation := range []string{"uplink.database.open", "uplink.token.create", "uplink.register", "uplink.token.activate", "uplink.cert.issue"} {
		count := 0
		for _, called := range *calls {
			if called == operation {
				count++
			}
		}
		if count != 4 {
			t.Errorf("operation %s crossed the real gate %d times, want 4", operation, count)
		}
	}
	db, err := sql.Open("sqlite", config.DatabasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = db.Close() }()
	var receipts int
	if err := db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_actions WHERE action='uplink.first-admin'`).Scan(&receipts); err != nil || receipts != 3 {
		t.Fatal("missing atomic first-run/redelivery/recovery audit receipts", receipts, err)
	}
}
