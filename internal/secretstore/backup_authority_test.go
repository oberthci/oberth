package secretstore_test

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/secretstore"
	"github.com/oberthci/oberth/internal/secretstore/fake"
)

type backupQuorum struct {
	t              *testing.T
	backend        *fake.Store
	mode           string
	cancel         context.CancelFunc
	granted        bool
	revocationSent bool
}

func TestBackupSetupNeedsNoRetainedRootAndRevokesQuorumAuthority(t *testing.T) {
	for _, mode := range []string{"success", "drift", "cancel", "wrong-share", "in-progress", "revoke-failed", "unpadded", "unpadded-drift", "disabled"} {
		t.Run(mode, func(t *testing.T) { backupQuorumCase(t, mode) })
	}
}

func backupQuorumCase(t *testing.T, mode string) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	backend := &backupQuorum{t: t, backend: &fake.Store{}, mode: mode, cancel: cancel}
	if mode == "drift" || mode == "unpadded-drift" {
		backend.backend.Resources = map[string]map[string]any{"/auth/kubernetes/role/oberth-backup": {"audience": "different"}}
	}
	client, err := secretstore.New(secretstore.Connection{URL: "https://store.test", Transport: backend})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.SetupBackupWithUnseal(ctx, config(), []byte("saved-share-fixture"))
	checkQuorumOutcome(t, backend, err)
}

func checkQuorumOutcome(t *testing.T, backend *backupQuorum, err error) {
	t.Helper()
	mode := backend.mode
	succeeds := mode == "success" || mode == "unpadded"
	if (err == nil) != succeeds {
		t.Fatal("quorum backup setup did not honor its outcome", mode, err)
	}
	if backend.granted != backend.revocationSent {
		t.Fatal("received quorum authority was not revoked", mode)
	}
	if succeeds && len(backend.backend.Resources) != 2 {
		t.Fatal("quorum setup did not create the two backup resources")
	}
	text := fmt.Sprint(err)
	if strings.Contains(text, "saved-share-fixture") || strings.Contains(text, backend.authority()) {
		t.Fatal("quorum setup exposed private material")
	}
	if mode == "disabled" && (backend.granted || !strings.Contains(text, "disable_unauthed_generate_root_endpoints = false")) {
		t.Fatal("disabled root generation did not name the store setting", err)
	}
}

// The store encodes a 26-byte root token without padding.
func (s *backupQuorum) authority() string {
	if strings.HasPrefix(s.mode, "unpadded") {
		return strings.Repeat("r", 26)
	}
	return strings.Repeat("r", 32)
}

func (s *backupQuorum) RoundTrip(r *http.Request) (*http.Response, error) {
	authority := s.authority()
	if strings.HasPrefix(r.URL.Path, "/v1/sys/generate-root/") {
		return s.generation(r)
	}
	if !s.granted || r.Header.Get("X-Vault-Token") != authority {
		s.t.Fatal("backup setup needed supplied root authority or widened its request")
	}
	if r.URL.Path == "/v1/auth/token/revoke-self" {
		if r.Context().Err() != nil {
			s.t.Fatal("revocation inherited cancellation")
		}
		s.revocationSent = true
		if s.mode == "revoke-failed" {
			return quorumReply(503, nil), nil
		}
		return quorumReply(204, nil), nil
	}
	if r.URL.Path != "/v1/sys/policies/acl/oberth-backup" && r.URL.Path != "/v1/auth/kubernetes/role/oberth-backup" {
		s.t.Fatal("quorum setup touched unrelated store resources")
	}
	return s.backend.RoundTrip(r)
}

func (s *backupQuorum) generation(r *http.Request) (*http.Response, error) {
	if r.Header.Get("X-Vault-Token") != "" || r.Method != "PUT" {
		s.t.Fatal("quorum authorization required an existing session")
	}
	if s.mode == "disabled" {
		return quorumReply(405, map[string]any{"errors": []string{"unsupported operation"}}), nil
	}
	if s.mode == "in-progress" {
		return quorumReply(400, map[string]any{"errors": []string{"private-other-attempt"}}), nil
	}
	pad := strings.Repeat("a", len(s.authority()))
	if r.URL.Path == "/v1/sys/generate-root/attempt" {
		return quorumReply(200, map[string]any{"nonce": "owned-attempt", "required": 1, "otp": pad}), nil
	}
	var request map[string]string
	if json.NewDecoder(r.Body).Decode(&request) != nil || request["nonce"] != "owned-attempt" || request["key"] != "saved-share-fixture" {
		s.t.Fatal("quorum share did not bind the operation nonce")
	}
	if s.mode == "wrong-share" {
		return quorumReply(403, map[string]any{"errors": []string{"private-share-refusal"}}), nil
	}
	encoded := make([]byte, len(pad))
	subtle.XORBytes(encoded, []byte(s.authority()), []byte(pad))
	s.granted = true
	if s.mode == "cancel" {
		s.cancel()
	}
	encoding := base64.StdEncoding
	if strings.HasPrefix(s.mode, "unpadded") {
		encoding = base64.RawStdEncoding
	}
	return quorumReply(200, map[string]any{"nonce": "owned-attempt", "complete": true, "encoded_token": encoding.EncodeToString(encoded)}), nil
}

func quorumReply(status int, value any) *http.Response {
	raw, _ := json.Marshal(value)
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(string(raw)))}
}
