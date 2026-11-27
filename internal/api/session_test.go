package api_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/apicontract"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func discover(f *fixture) *httptest.ResponseRecorder {
	return request(f, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"server/discover"}`))
}
func TestCertificateTrustExpiryAndDualAuthority(t *testing.T) {
	f := setup(t, nil)
	now := f.clock.Now()
	leaf := &x509.Certificate{Raw: []byte("verified-leaf"), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}
	for _, tc := range []struct {
		name   string
		state  *tls.ConnectionState
		bearer string
		want   int
	}{
		{"verified", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}, "", 200},
		{"unverified", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}, "valid", 401},
		{"dual", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}, "valid", 200},
		{"bad bearer", &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}, "invalid", 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/api/status", nil)
			r.TLS = tc.state
			if tc.bearer != "" {
				r.Header.Set("Authorization", "Bearer "+tc.bearer)
			}
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, modernRequest(r))
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	f.clock.Advance(time.Hour)
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 401 {
		t.Fatal("expired certificate accepted")
	}
}
func TestUnknownCertificateNeverFallsBackToBearer(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Auth = splitAuth{bearer: admin()} })
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.Header.Set("Authorization", "Bearer valid")
	leaf := &x509.Certificate{Raw: []byte("leaf"), NotBefore: f.clock.Now().Add(-time.Hour), NotAfter: f.clock.Now().Add(time.Hour)}
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, modernRequest(r))
	if w.Code != 401 {
		t.Fatal("certificate fell back")
	}
}

type splitAuth struct {
	bearer      apicontract.Caller
	certificate apicontract.Caller
}

func (a splitAuth) Bearer(context.Context, string) (apicontract.Caller, error) { return a.bearer, nil }
func (a splitAuth) Certificate(context.Context, *x509.Certificate) (apicontract.Caller, error) {
	if a.certificate.CredentialID == "" {
		return apicontract.Caller{}, publicerr.New(publicerr.Forbidden, "Certificate is unknown.", "Enroll a current certificate.", nil)
	}
	return a.certificate, nil
}
func TestBodyOnlyAdminAndIsolatedEnrollment(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/api/admin/uplink-add?identity=agent", "/api/admin/unknown"} {
		if w := request(f, "POST", path, []byte(`{"identity":"agent"}`)); w.Code == 200 {
			t.Fatal("unregistered or query administration accepted")
		}
	}
	for _, method := range []string{"tools/list", "ping"} {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":%q}`, method)
		w := request(f, "POST", "/mcp/enroll", []byte(body))
		assertSuccess(t, w)
		if w.Header().Get("Mcp-Session-Id") != "" || strings.Contains(w.Body.String(), "run_get") {
			t.Fatal("enrollment exposed session or ordinary inventory")
		}
	}
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"uplink_redeem","arguments":{"secret":"private-invitation","csr":"private-csr","server_url":"https://oberth.ci/mcp","ca_pin":"pin","ssh_public_key":"public"}}}`
	w := request(f, "POST", "/mcp/enroll", []byte(body))
	assertSuccess(t, w)
	if w.Header().Get("Mcp-Session-Id") != "" || strings.Contains(w.Body.String(), "private-") {
		t.Fatal("enrollment exposed session or credential")
	}
}
func TestAllConfigBounds(t *testing.T) {
	for _, tc := range []struct {
		key   string
		limit int
	}{{"wait", 600}} {
		for _, delta := range []int{0, 1} {
			b := fake.Bindings()
			r, err := api.Builtin(b)
			if err != nil {
				t.Fatal(err)
			}
			c := api.Config{FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: r, Auth: fake.NewAuth(admin()), Runtime: fake.NewClock().Runtime(), Classify: publicerr.Classify}
			switch tc.key {
			case "wait":
				c.MaximumWait = time.Duration(tc.limit+delta) * time.Second
			}
			_, err = api.New(c)
			if (err != nil) != (delta == 1) {
				t.Fatalf("config %s %d: %v", tc.key, delta, err)
			}
		}
	}
}

func TestDualCredentialsRejectEveryAuthorityMismatch(t *testing.T) {
	for _, field := range []string{"identity", "fingerprint", "admin", "scope"} {
		t.Run(field, func(t *testing.T) {
			cert := admin()
			switch field {
			case "identity":
				cert.Actor.Identity = "other"
			case "fingerprint":
				cert.Actor.Fingerprint = "other"
			case "admin":
				cert.Actor.Admin = false
			case "scope":
				cert.Actor.Scope = []string{"origin"}
			}
			f := setup(t, func(c *api.Config, _ *apicontract.Bindings) { c.Auth = splitAuth{bearer: admin(), certificate: cert} })
			leaf := &x509.Certificate{Raw: []byte("leaf"), NotBefore: f.clock.Now().Add(-time.Hour), NotAfter: f.clock.Now().Add(time.Hour)}
			r := httptest.NewRequest("GET", "/api/status", nil)
			r.Header.Set("Authorization", "Bearer valid")
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, modernRequest(r))
			if w.Code != 401 {
				t.Fatal("conflicting authority accepted")
			}
		})
	}
}
func TestSourceListenerHasNoHistoryAuthority(t *testing.T) {
	f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
		c.Source = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/load" {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write([]byte("source bytes"))
		})
	})
	for _, tc := range []struct {
		path string
		want int
	}{{"/load", 200}, {"/api/runs", 404}, {"/mcp", 404}} {
		w := httptest.NewRecorder()
		f.server.SourceHandler().ServeHTTP(w, httptest.NewRequest("GET", tc.path, nil))
		if w.Code != tc.want {
			t.Fatal("listeners mixed authority")
		}
	}
	if w := request(f, "GET", "/load", nil); w.Code != 404 {
		t.Fatal("source mounted on user listener")
	}
}

func TestToolDeadlineIncludesPreparationAndListenerUsesTheLongestTool(t *testing.T) {
	var preparedDeadline time.Time
	h := apicontract.HandlerFunc[apicontract.RunWaitArgs, apicontract.WaitResult](func(ctx context.Context, _ apicontract.Caller, _ apicontract.RunWaitArgs) (apicontract.Call[apicontract.WaitResult], error) {
		preparedDeadline, _ = ctx.Deadline()
		return func(ctx context.Context) (apicontract.WaitResult, error) {
			deadline, _ := ctx.Deadline()
			if !deadline.Equal(preparedDeadline) {
				t.Error("execution restarted the timeout budget")
			}
			return apicontract.WaitResult{}, nil
		}, nil
	})
	f := setup(t, func(_ *api.Config, b *apicontract.Bindings) { b.RunWait.Handler = h })
	assertSuccess(t, rpc(f, "run_wait", map[string]any{"id": "12345678", "timeout_s": 1}))
	if preparedDeadline.IsZero() || f.server.WriteTimeout() != 16*time.Minute {
		t.Fatal("deadline metadata did not reach the listener contract")
	}
}

func TestMCPFramingRefusalsPreserveProtocolCodeAndRequestID(t *testing.T) {
	for _, tc := range []struct {
		body string
		code int
		id   string
	}{
		{`{`, -32700, `null`},
		{`{"jsonrpc":"1.0","id":"stable","method":"ping"}`, -32600, `null`},
		{`{"jsonrpc":"2.0","id":"stable","method":"unknown"}`, -32601, `"stable"`},
		{`{"jsonrpc":"2.0","id":"stable","method":"tools/call","params":{"name":"run_get","extra":true}}`, -32602, `"stable"`},
		{`{"jsonrpc":"2.0","id":"stable","method":"tools/call","params":{"name":"unknown"}}`, -32602, `"stable"`},
	} {
		f := setup(t, nil)
		w := request(f, "POST", "/mcp", []byte(tc.body))
		var reply struct {
			ID    json.RawMessage `json:"id"`
			Error struct {
				Code int `json:"code"`
			} `json:"error"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Error.Code != tc.code || string(reply.ID) != tc.id {
			t.Fatalf("protocol refusal changed: %s", w.Body.String())
		}
	}
}
