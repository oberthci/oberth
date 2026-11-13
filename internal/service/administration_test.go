package service

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/model"
)

type administrationAuth struct{}

func (administrationAuth) Authenticate(_ context.Context, token string) (api.Actor, error) {
	switch token {
	case "admin":
		return api.Actor{Identity: "admin@host", Fingerprint: "admin", Admin: true}, nil
	case "user":
		return api.Actor{Identity: "user@host", Fingerprint: "user"}, nil
	case "scoped":
		return api.Actor{Identity: "scoped@host", Fingerprint: "scoped", Admin: true, Scope: []string{"north"}}, nil
	}
	return api.Actor{}, api.ErrInvalidToken
}
func (a administrationAuth) AuthenticateCertificate(ctx context.Context, cert *x509.Certificate) (api.Actor, error) {
	return a.Authenticate(ctx, cert.Subject.CommonName)
}

type administrationAudit struct {
	records                 []model.AuditActionSpec
	failIntent, failOutcome bool
}

func (a *administrationAudit) AppendAuditAction(_ context.Context, spec model.AuditActionSpec) (model.AuditAction, error) {
	if a.failIntent && strings.HasSuffix(spec.Action, ".intent") || a.failOutcome && strings.HasSuffix(spec.Action, ".succeeded") {
		return model.AuditAction{}, errors.New("audit unavailable")
	}
	a.records = append(a.records, spec)
	return model.AuditAction{}, nil
}

type adminTestCase struct{ action, name, body string }

func administrationCases(t *testing.T) []adminTestCase {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(key, "")
	if err != nil {
		t.Fatal(err)
	}
	keyBody, err := json.Marshal(api.UpstreamKeyRequest{Name: "south", PrivateKey: string(pem.EncodeToMemory(block))})
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	enroll, err := json.Marshal(api.UplinkAddRequest{Identity: "new-operator@host", SSHPublicKey: string(ssh.MarshalAuthorizedKey(signer.PublicKey())), Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	return []adminTestCase{
		{"uplink-add", "uplink_add", string(enroll)},
		{"uplink-scope", "uplink_scope", `{"identity":"operator@host","scope":[]}`},
		{"upstream-add", "upstream_add", `{"name":"south","base_url":"ssh://source.invalid/team"}`},
		{"upstream-list", "upstream_list", `{"check":true}`},
		{"upstream-remove", "upstream_remove", `{"name":"remove-me"}`},
		{"upstream-provide-key", "upstream_provide_key", string(keyBody)},
		{"repo-add", "repo_add", `{"repo":"new-repo","upstream":"south"}`},
		{"repo-verify", "repo_verify", `{"repo":"new-repo"}`},
		{"access-list", "admin_access_list", `{}`},
		{"access-allow", "admin_access_allow", `{"repo":"new-repo","workflow":".oberth/build.yaml","step":"build","secret":"oberth/upstream/team/new-repo/a"}`},
		{"access-revoke", "admin_access_revoke", `{"repo":"new-repo","workflow":".oberth/build.yaml","step":"build","secret":"oberth/upstream/team/new-repo/a"}`},
		{"public-identity", "admin_public_identity", `{"name":"server-ca"}`},
		{"readiness", "admin_readiness", `{}`},
		{"version", "admin_version", `{}`},
		{"idle", "admin_idle", `{"wait_for":"all"}`},
		{"audit-record", "admin_audit_record", `{"action":"upgrade.completed","details":{"to":"test"}}`},
		{"database-backup", "admin_database_backup", `{}`},
	}
}

func newAdministrationFixture(t *testing.T) (*API, *administrationAudit, http.Handler) {
	t.Helper()
	fixture := newControlFixture(t)
	control := fixture.api(t)
	credential, err := fixture.store.CreateTokenCredential(t.Context(), model.TokenCredentialSpec{Name: "scope-fixture", Digest: make([]byte, 32)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RegisterUplink(t.Context(), "admin@host", model.UplinkSpec{Identity: "operator@host", Fingerprint: "SHA256:" + strings.Repeat("A", 43), TokenCredentialID: credential.ID}); err != nil {
		t.Fatal(err)
	}
	upstream, err := fixture.store.RegisterUpstream(t.Context(), "admin@host", model.UpstreamSpec{Name: "south", Kind: "ssh", BaseURL: "ssh://source.invalid/team"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RegisterRepository(t.Context(), "admin@host", model.RepositorySpec{Name: "new-repo", UpstreamID: upstream.ID, DefaultBranch: "main"}); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.store.RegisterUpstream(t.Context(), "admin@host", model.UpstreamSpec{Name: "remove-me", Kind: "ssh", BaseURL: "ssh://source.invalid/unused"}); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.OpenIdleQueueReaders(t.Context()); err != nil {
		t.Fatal(err)
	}
	audit := &administrationAudit{}
	control.auditor = audit
	control.secretAccess = fixture.store
	control.administration = Administration{
		Version: "test",
		EnsureUpstream: func(context.Context, api.UpstreamAddRequest) (api.UpstreamIdentity, error) {
			return api.UpstreamIdentity{Pending: true}, nil
		},
		ImportUpstreamKey: func(context.Context, api.UpstreamKeyRequest) (api.UpstreamIdentity, error) {
			return api.UpstreamIdentity{}, nil
		},
		CheckUpstream:    func(context.Context, model.Upstream) (string, error) { return "ok", nil },
		VerifyRepository: func(context.Context, string) (any, error) { return map[string]any{"verified": true}, nil },
		DatabaseBackup: func(_ context.Context, _ int64) (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader("verified archive fixture")), nil
		},
	}
	server, err := api.New(administrationAuth{}, control, control, "test", api.WithErrorClassifier(func(err error) (int, string) {
		switch {
		case errors.Is(err, ErrForbidden):
			return http.StatusForbidden, "forbidden"
		case errors.Is(err, ErrInvalidInput):
			return http.StatusBadRequest, "invalid input"
		case errors.Is(err, ErrUnavailable):
			return http.StatusServiceUnavailable, "unavailable"
		default:
			return http.StatusInternalServerError, "failed"
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	return control, audit, server.Handler()
}

var administrationRequestID atomic.Uint32

func adminRequest(handler http.Handler, action, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/api/admin/"+action, strings.NewReader(body))
	// Isolate anonymous endpoint checks from the existing per-source failure budget.
	req.RemoteAddr = fmt.Sprintf("192.0.2.%d:1234", administrationRequestID.Add(1)%250+1)
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	return out
}

func testAdministrationRequiresAdmin(t *testing.T) {
	control, audit, handler := newAdministrationFixture(t)
	for _, test := range administrationCases(t) {
		t.Run(test.action, func(t *testing.T) {
			for _, token := range []string{"", "user", "scoped"} {
				out := adminRequest(handler, test.action, token, test.body)
				want := http.StatusForbidden
				if token == "" {
					want = http.StatusUnauthorized
				}
				if out.Code != want {
					t.Errorf("token %q: got %d, want %d: %s", token, out.Code, want, out.Body)
				}
			}
			for _, actor := range []api.Actor{{}, {Identity: "user@host"}, {Identity: "admin@host", Admin: true, Scope: []string{"north"}}} {
				if test.name == "admin_database_backup" {
					if stream, err := control.PrepareAdminBackup(t.Context(), actor, json.RawMessage(test.body)); err == nil {
						_ = stream.Close()
						t.Error("direct backup accepted non-admin")
					}
				} else if _, err := control.CallTool(t.Context(), actor, test.name, json.RawMessage(test.body)); err == nil {
					t.Error("direct MCP call accepted non-admin")
				}
			}
		})
	}
	if len(audit.records) != 0 {
		t.Fatal("unauthorized administration reached audit or operation")
	}
}

func testAdministrationRejectsPaths(t *testing.T) {
	control, _, handler := newAdministrationFixture(t)
	externalCalls := 0
	control.administration.EnsureUpstream = func(context.Context, api.UpstreamAddRequest) (api.UpstreamIdentity, error) {
		externalCalls++
		return api.UpstreamIdentity{}, nil
	}
	control.administration.ImportUpstreamKey = func(context.Context, api.UpstreamKeyRequest) (api.UpstreamIdentity, error) {
		externalCalls++
		return api.UpstreamIdentity{}, nil
	}
	control.administration.VerifyRepository = func(context.Context, string) (any, error) {
		externalCalls++
		return map[string]any{"verified": true}, nil
	}
	keyMaterial := ""
	publicKey := ""
	for _, test := range administrationCases(t) {
		if test.action == "uplink-add" {
			var args api.UplinkAddRequest
			if json.Unmarshal([]byte(test.body), &args) != nil {
				t.Fatal("bad enrollment fixture")
			}
			publicKey = args.SSHPublicKey
		}
		if test.action == "upstream-provide-key" {
			var args api.UpstreamKeyRequest
			if err := json.Unmarshal([]byte(test.body), &args); err != nil {
				t.Fatal(err)
			}
			keyMaterial = args.PrivateKey
		}
	}
	for _, path := range []string{"../private", "/new-repo", "/etc/private", "..\\private", "%2e%2e%2fprivate", "name/../../private"} {
		fields := []struct{ action, field string }{
			{"uplink-add", "ssh_public_key"}, {"uplink-add", "csr"}, {"public-identity", "name"}, {"upstream-add", "name"}, {"upstream-remove", "name"},
			{"upstream-provide-key", "name"}, {"repo-add", "repo"}, {"repo-add", "upstream"}, {"repo-add", "default_branch"}, {"repo-verify", "repo"},
			{"upstream-add", "check_repo"}, {"upstream-add", "base_url"}, {"upstream-provide-key", "private_key"},
		}
		for _, field := range fields {
			args := map[string]any{"name": "south", "base_url": "ssh://source.invalid/team"}
			switch field.action {
			case "uplink-add":
				args = map[string]any{"identity": "new-operator@host", "ssh_public_key": publicKey}
			case "public-identity", "upstream-remove":
				args = map[string]any{"name": "server-ca"}
			case "upstream-provide-key":
				args = map[string]any{"name": "south", "private_key": keyMaterial}
			case "repo-add":
				args = map[string]any{"repo": "new-repo", "upstream": "south"}
			case "repo-verify":
				args = map[string]any{"repo": "new-repo"}
			}
			args[field.field] = path
			body, err := json.Marshal(args)
			if err != nil {
				t.Fatal(err)
			}
			out := adminRequest(handler, field.action, "admin", string(body))
			if out.Code != http.StatusBadRequest {
				t.Errorf("%s %s=%q: got %d: %s", field.action, field.field, path, out.Code, out.Body)
			}
		}
	}
	if externalCalls != 0 {
		t.Errorf("path-like inputs reached %d external operations", externalCalls)
	}
	for _, test := range administrationCases(t) {
		out := adminRequest(handler, test.action, "admin", `{"path":"/etc/private"}`)
		if out.Code != http.StatusBadRequest {
			t.Errorf("%s accepted path: %d", test.action, out.Code)
		}
	}
}

func testAdministrationRejectsMalformedBodies(t *testing.T) {
	control, _, handler := newAdministrationFixture(t)
	for _, test := range administrationCases(t) {
		for _, body := range []string{"", `{`, `null`, `[]`, `{} {}`, `{"name":"a","name":"b"}`, `{"x":"` + strings.Repeat("x", api.AdminRequestBytes) + `"}`} {
			out := adminRequest(handler, test.action, "admin", body)
			want := http.StatusBadRequest
			if len(body) > api.AdminRequestBytes {
				want = http.StatusRequestEntityTooLarge
			}
			if out.Code != want {
				t.Errorf("%s body size %d: got %d, want %d", test.action, len(body), out.Code, want)
			}
			if test.name != "admin_database_backup" {
				if _, err := control.CallTool(t.Context(), api.Actor{Identity: "admin@host", Admin: true}, test.name, json.RawMessage(body)); !errors.Is(err, ErrInvalidInput) {
					t.Errorf("MCP %s malformed: %v", test.name, err)
				}
			}
		}
	}
}

func testAdministrationEveryActionAudited(t *testing.T) {
	_, audit, handler := newAdministrationFixture(t)
	for _, test := range administrationCases(t) {
		start := len(audit.records)
		out := adminRequest(handler, test.action, "admin", test.body)
		records := audit.records[start:]
		if len(records) < 2 || records[0].Action != test.name+".intent" {
			t.Errorf("%s missing intent: %+v (HTTP %d)", test.action, records, out.Code)
			continue
		}
		outcome := ".succeeded"
		if out.Code != http.StatusOK {
			outcome = ".failed"
		}
		if records[len(records)-1].Action != test.name+outcome {
			t.Errorf("%s missing outcome: %+v", test.action, records)
		}
		for _, record := range records {
			if record.Actor != "admin@host" {
				t.Errorf("unbound actor: %+v", record)
			}
			if strings.Contains(record.Details, "PRIVATE KEY") || strings.Contains(record.Details, "private_key") {
				t.Error("key material entered audit")
			}
		}
	}
}

func testAdministrationAuditFailureClosesActions(t *testing.T) {
	_, audit, handler := newAdministrationFixture(t)
	for _, test := range administrationCases(t) {
		audit.failIntent = true
		out := adminRequest(handler, test.action, "admin", test.body)
		if out.Code == http.StatusOK {
			t.Errorf("%s succeeded with failed audit", test.action)
		}
		if len(audit.records) != 0 {
			t.Errorf("%s continued after failed intent", test.action)
		}
	}
	audit.failIntent = false
	audit.failOutcome = true
	out := adminRequest(handler, "database-backup", "admin", `{}`)
	if out.Code == http.StatusOK || strings.Contains(out.Body.String(), "archive fixture") {
		t.Error("backup exposed bytes despite failed outcome audit")
	}
}

func testAdministrationAcceptsVerifiedAdminCertificate(t *testing.T) {
	_, audit, handler := newAdministrationFixture(t)
	cert := &x509.Certificate{NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	cert.Subject.CommonName = "admin"
	req := httptest.NewRequest(http.MethodPost, "/api/admin/version", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	out := httptest.NewRecorder()
	handler.ServeHTTP(out, req)
	if out.Code != http.StatusOK || len(audit.records) != 2 {
		t.Fatalf("admin certificate: %d %s", out.Code, out.Body)
	}
}

func testAdministrationPublicFileConfinement(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tls.crt")
	if err := os.WriteFile(path, []byte("public certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	body, err := readPublicIdentityFile(t.Context(), path)
	if err != nil || string(body) != "public certificate" {
		t.Fatalf("public read: %q %v", body, err)
	}
	outside := filepath.Join(t.TempDir(), "tls.crt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if _, err := readPublicIdentityFile(t.Context(), path); err == nil {
		t.Fatal("public read escaped its identity directory")
	}
}

func TestAdministrationSecurity(t *testing.T) {
	t.Run("RequiresAdmin", testAdministrationRequiresAdmin)
	t.Run("RejectsPaths", testAdministrationRejectsPaths)
	t.Run("RejectsMalformedBodies", testAdministrationRejectsMalformedBodies)
	t.Run("EveryActionAudited", testAdministrationEveryActionAudited)
	t.Run("AuditFailureClosesActions", testAdministrationAuditFailureClosesActions)
	t.Run("AcceptsVerifiedAdminCertificate", testAdministrationAcceptsVerifiedAdminCertificate)
	t.Run("PublicFileConfinement", testAdministrationPublicFileConfinement)
	t.Run("UplinkScopeUpdates", testAdministrationUplinkScopeUpdates)
	t.Run("RequiresJSONContentType", testAdministrationRequiresJSONContentType)
	t.Run("CertificateOnlyEnrollment", testAdministrationCertificateOnlyEnrollment)
}

func testAdministrationUplinkScopeUpdates(t *testing.T) {
	_, _, handler := newAdministrationFixture(t)
	for _, scope := range [][]string{{"south"}, {"metrics"}, {}} {
		body, err := json.Marshal(map[string]any{"identity": "operator@host", "scope": scope})
		if err != nil {
			t.Fatal(err)
		}
		out := adminRequest(handler, "uplink-scope", "admin", string(body))
		var result struct {
			Scope []string `json:"scope"`
		}
		if out.Code != http.StatusOK || json.Unmarshal(out.Body.Bytes(), &result) != nil || strings.Join(result.Scope, ",") != strings.Join(scope, ",") || result.Scope == nil {
			t.Fatalf("scope %v: %d %s", scope, out.Code, out.Body)
		}
	}
}

func testAdministrationRequiresJSONContentType(t *testing.T) {
	_, _, handler := newAdministrationFixture(t)
	for _, test := range administrationCases(t) {
		for _, contentType := range []string{"", "text/plain", "application/x-www-form-urlencoded"} {
			req := httptest.NewRequest(http.MethodPost, "/api/admin/"+test.action, strings.NewReader(test.body))
			req.Header.Set("Authorization", "Bearer admin")
			if contentType != "" {
				req.Header.Set("Content-Type", contentType)
			}
			out := httptest.NewRecorder()
			handler.ServeHTTP(out, req)
			if out.Code != http.StatusUnsupportedMediaType {
				t.Errorf("%s content type %q: %d %s", test.action, contentType, out.Code, out.Body)
			}
		}
	}
}

func testAdministrationCertificateOnlyEnrollment(t *testing.T) {
	control, _, handler := newAdministrationFixture(t)
	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	defer clear(files["ca.key"])
	authority, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	database, ok := control.runs.(ClientCertStore)
	if !ok {
		t.Fatal("missing client certificate store")
	}
	control.clientCertificates = &ClientCertIssuer{Authority: authority, Store: database}
	public, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key)
	sshKey, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	request := api.UplinkAddRequest{Identity: "new-admin@host", SSHPublicKey: string(ssh.MarshalAuthorizedKey(sshKey)), CSR: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr})), Admin: true}
	raw, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response := adminRequest(handler, "uplink-add", "admin", string(raw))
	if response.Code != http.StatusOK {
		t.Fatal(response.Code, response.Body.String())
	}
	var result api.UplinkAddResult
	if json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Token != "" || result.Certificate == "" {
		t.Fatal("certificate-only enrollment returned an invalid credential result")
	}
	leafBlock, _ := pem.Decode([]byte(result.Certificate))
	if leafBlock == nil {
		t.Fatal("missing public certificate")
	}
	leaf, err := x509.ParseCertificate(leafBlock.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if err := leaf.CheckSignatureFrom(authority.Certificate); err != nil {
		t.Fatal(err)
	}
	if _, err := database.CertificateUplink(t.Context(), request.Identity, result.Fingerprint); err != nil {
		t.Fatal("enrollment was not registered", err)
	}
	request.Identity = "invalid-lifetime@host"
	request.TTL = "15m"
	raw, err = json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	response = adminRequest(handler, "uplink-add", "admin", string(raw))
	if response.Code != http.StatusBadRequest {
		t.Fatal("invalid lifetime accepted", response.Code)
	}
	if _, err := database.CertificateUplink(t.Context(), request.Identity, result.Fingerprint); err == nil {
		t.Fatal("invalid certificate request created an uplink")
	}
}

func TestAdministrationRepoVerifySelectors(t *testing.T) {
	control, _, handler := newAdministrationFixture(t)
	called := ""
	control.administration.VerifyRepository = func(_ context.Context, repo string) (any, error) {
		called = repo
		return map[string]bool{"verified": true}, nil
	}
	for _, selector := range []string{"new-repo", "south/team/new-repo"} {
		called = ""
		body, _ := json.Marshal(map[string]string{"repo": selector})
		response := adminRequest(handler, "repo-verify", "admin", string(body))
		if response.Code != http.StatusOK || called != "south/team/new-repo" {
			t.Fatalf("selector %s: status=%d target=%s", selector, response.Code, called)
		}
	}
	called = ""
	response := adminRequest(handler, "repo-verify", "admin", `{"repo":"other/team/new-repo"}`)
	if response.Code == http.StatusOK || called != "" {
		t.Fatal("another upstream selector reached verification")
	}
}
