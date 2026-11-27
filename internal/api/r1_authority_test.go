package api_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/publicerr"
)

// This port accepts arbitrary tokens so transport syntax cannot be masked by a fake.
type authProbe struct {
	calls    int
	deadline time.Time
}

func (a *authProbe) Bearer(ctx context.Context, _ string) (apicontract.Caller, error) {
	a.calls++

	a.deadline, _ = ctx.Deadline()
	return admin(), nil
}
func (a *authProbe) Certificate(context.Context, *x509.Certificate) (apicontract.Caller, error) {
	return admin(), nil
}
func TestMalformedCredentialsNeverReachLookup(t *testing.T) {
	for _, headers := range [][]string{{"Bearer valid", "Bearer valid"}, {"Bearer valid bad"}, {"Bearer valid,bad"}, {"Bearer valid\tbad"}} {
		probe := &authProbe{}
		f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Auth = probe })
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.Header["Authorization"] = headers
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		if w.Code != 401 || probe.calls != 0 {
			t.Fatal("transport credential syntax bypassed", w.Code, probe.calls)
		}
	}
	probe := &authProbe{}
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Auth = probe })
	parent, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	outer, _ := parent.Deadline()
	r := httptest.NewRequest("GET", "/api/status", nil).WithContext(parent)
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	assertSuccess(t, w)
	if d := outer.Sub(probe.deadline); d < 4*time.Second || d > 6*time.Second {
		t.Fatal("lookup deadline changed", d)
	}
}
func TestAuthenticationRequiresStableCredentialID(t *testing.T) {
	f := setup(t, nil)
	c := admin()
	c.CredentialID = ""
	f.auth.Set(c, nil)
	if w := request(f, "GET", "/api/status", nil); w.Code != 503 {
		t.Fatal("missing credential ID accepted", w.Body.String())
	}
}
func TestCertificateNotBeforeIncludesExactBoundary(t *testing.T) {
	for _, offset := range []time.Duration{0, time.Nanosecond} {
		f := setup(t, nil)
		now := f.clock.Now()
		leaf := &x509.Certificate{NotBefore: now.Add(offset), NotAfter: now.Add(time.Hour)}
		r := httptest.NewRequest("GET", "/api/status", nil)
		r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
		w := httptest.NewRecorder()
		f.server.Handler().ServeHTTP(w, modernRequest(r))
		want := 200
		if offset > 0 {
			want = 401
		}
		if w.Code != want {
			t.Fatal("certificate validity boundary", offset, w.Code)
		}
	}
}
func TestAuthorityMatrixIncludesMetricsAndBackup(t *testing.T) {
	for _, tc := range []struct {
		admin   bool
		scope   []string
		metrics bool
		backup  bool
	}{{false, nil, false, false}, {true, []string{"widget"}, false, false}, {false, []string{"metrics"}, true, false}, {true, nil, true, true}} {
		f := setup(t, nil)
		c := admin()
		c.Actor.Admin = tc.admin
		c.Actor.Scope = tc.scope
		f.auth.Set(c, nil)
		w := request(f, "GET", "/metrics", nil)
		if (w.Code == 200) != tc.metrics {
			t.Fatal("metrics authority", tc, w.Code)
		}
		w = request(f, "POST", "/api/admin/database-backup", []byte(`{}`))
		if (w.Code == 200) != tc.backup {
			t.Fatal("backup authority", tc, w.Code)
		}
	}
	f := setup(t, nil)
	c := admin()
	c.Actor.Scope = []string{"metrics"}
	f.auth.Set(c, nil)
	for _, path := range []string{"/api/runs/counts", "/api/runs/12345678/containers", "/api/runs/12345678/containerlog?node_id=node&container=main", "/api/runs/12345678/containerlog/stream?node_id=node&container=main"} {
		if w := request(f, "GET", path, nil); w.Code != 403 {
			t.Fatal("metrics entered private view", path, w.Code)
		}
	}
	if w := rpc(f, "run_list", map[string]any{}); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("metrics caller invoked a tool")
	}
}
func TestAdminRoutesOnlyExposeDeclaredRows(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/api/admin/run-get", "/api/admin/run-cancel", "/api/admin/promote", "/api/admin/client-renew"} {
		if w := request(f, "POST", path, []byte(`{}`)); w.Code != 404 {
			t.Fatal("undeclared admin alias", path, w.Code)
		}
	}
	c := admin()
	c.Actor.Admin = false
	f.auth.Set(c, nil)
	if w := request(f, "POST", "/api/admin/access-list", []byte(`{}`)); w.Code != 403 {
		t.Fatal("administrative listing allowed")
	}
}
func TestDuplicateQueryRefusesBeforeRead(t *testing.T) {
	f := setup(t, nil)
	w := request(f, "GET", "/api/runs?limit=1&limit=2", nil)
	if w.Code != 400 {
		t.Fatal("ambiguous query accepted", w.Code)
	}
	h := f.bindings.RunList.Handler.(*fake.Handler[apicontract.RunListArgs, apicontract.RunPage])
	if inputs, _, _ := h.Observed(); len(inputs) != 0 {
		t.Fatal("ambiguous query reached service")
	}
}
func TestEnrollmentPreservesAvailabilityAndInvitationRefusal(t *testing.T) {
	for _, cause := range []error{publicerr.New(publicerr.Forbidden, "Not available.", "Try later.", nil), publicerr.New(publicerr.Unavailable, "Service is unavailable.", "Retry later.", nil), errors.New("private dependency")} {
		f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
			c.Enroll = &fake.Handler[model.InviteRedemption, model.InviteCertificate]{Error: cause}
		})
		w := request(f, "POST", "/mcp/enroll", []byte(redemptionBody()))
		want := "Invitation redemption was refused."
		if publicerr.Classify(cause).Code() == publicerr.Unavailable {
			want = "Service is unavailable."
		}
		if publicerr.Classify(cause).Code() == publicerr.Invariant {
			want = "Reference:"
		}
		if !strings.Contains(w.Body.String(), want) || strings.Contains(w.Body.String(), "private dependency") {
			t.Fatal(w.Body.String())
		}
	}
}
func redemptionBody() string {
	return `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uplink_redeem","arguments":{"secret":"invitation","csr":"request","server_url":"https://oberth.ci/mcp","ca_pin":"pin","ssh_public_key":"public"}}}`
}
func TestEnrollmentIsolation(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.Enroll = &fake.Handler[model.InviteRedemption, model.InviteCertificate]{Error: publicerr.New(publicerr.Forbidden, "Refused.", "Request an invitation.", nil)}
	})
	r := httptest.NewRequest("POST", "/mcp/enroll", strings.NewReader(redemptionBody()))
	r.Header.Set("Authorization", "Bearer valid")
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 404 {
		t.Fatal("ordinary credentials accepted by enrollment")
	}
	w = request(f, "POST", "/mcp/enroll", []byte(strings.Replace(redemptionBody(), "uplink_redeem", "run_get", 1)))
	if !strings.Contains(w.Body.String(), `"code":-32601`) {
		t.Fatal("ordinary tool accepted by enrollment")
	}
}

func TestScopedAdminCannotUseUnscopedTools(t *testing.T) {
	f := setup(t, nil)
	c := admin()
	c.Actor.Scope = []string{"widget"}
	f.auth.Set(c, nil)
	r, err := api.Builtin(f.bindings)
	if err != nil {
		t.Fatal(err)
	}
	for _, tool := range listTools(t, r, c) {
		if tool.Name == "admin_version" {
			t.Fatal("unscoped tool listed to scoped administrator")
		}
	}
	if w := rpc(f, "admin_version", map[string]any{}); !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal("scoped administrator invoked unscoped tool")
	}
	assertSuccess(t, rpc(f, "run_list", map[string]any{}))
}

func TestAdminActionQueryCannotReachHandler(t *testing.T) {
	f := setup(t, nil)
	w := request(f, "POST", "/api/admin/version?extra=1", []byte(`{}`))
	if w.Code != 400 {
		t.Fatal("admin query accepted", w.Code)
	}
	h := f.bindings.AdminVersion.Handler.(*fake.Handler[apicontract.AdminVersionArgs, apicontract.SystemStatus])
	if inputs, _, _ := h.Observed(); len(inputs) != 0 {
		t.Fatal("admin query reached service")
	}
}
