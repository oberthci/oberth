package api_test

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"github.com/oberthci/oberth/internal/apicontract"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/publicerr"
)

func initialize(f *fixture) *httptest.ResponseRecorder {
	return request(f, "POST", "/mcp", []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`))
}
func TestSession64Accepts65RefusesAndIdleExpiry(t *testing.T) {
	f := setup(t, nil)
	for i := 0; i < 64; i++ {
		assertSuccess(t, initialize(f))
	}
	if w := initialize(f); !strings.Contains(w.Body.String(), `"error":`) {
		t.Fatal("sixty-fifth session accepted")
	}
	f.clock.Advance(time.Hour - time.Nanosecond)
	if w := request(f, "DELETE", "/mcp", nil); w.Code != 204 {
		t.Fatal("session expired before its idle bound")
	}
	assertSuccess(t, initialize(f))
	f.clock.Advance(time.Hour)
	if w := request(f, "DELETE", "/mcp", nil); w.Code != 404 {
		t.Fatal("idle session retained")
	}
	assertSuccess(t, initialize(f))
}
func TestSessionCredentialBinding(t *testing.T) {
	f := setup(t, nil)
	assertSuccess(t, initialize(f))
	c := admin()
	c.CredentialID = "different-private-id"
	f.auth.Set(c, nil)
	if w := request(f, "DELETE", "/mcp", nil); w.Code != 404 {
		t.Fatal("session transferred authority")
	}
	c.CredentialID = "private-id"
	f.auth.Set(c, nil)
	if w := request(f, "DELETE", "/mcp", nil); w.Code != 204 {
		t.Fatal(w.Body.String())
	}
}
func TestRunResource64Accepts65Refuses(t *testing.T) {
	f := setup(t, nil)
	assertSuccess(t, initialize(f))
	for i := 0; i < 65; i++ {
		body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": i, "method": "resources/subscribe", "params": map[string]string{"uri": fmt.Sprintf("run://run-%08d", i)}})
		w := request(f, "POST", "/mcp", body)
		if strings.Contains(w.Body.String(), `"error":`) != (i == 64) {
			t.Fatalf("subscription %d: %s", i, w.Body.String())
		}
	}
}
func TestProtocolDatesAndRequestID(t *testing.T) {
	for _, tc := range []struct {
		version, want string
		refused       bool
	}{
		{"2025-03-26", "2025-03-26", false}, {"2025-06-18", "2025-06-18", false}, {"2030-01-01", "2025-06-18", false}, {"invalid", "", true},
	} {
		f := setup(t, nil)
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":"identity","method":"initialize","params":{"protocolVersion":%q}}`, tc.version)
		w := request(f, "POST", "/mcp", []byte(body))
		if !strings.Contains(w.Body.String(), `"id":"identity"`) || strings.Contains(w.Body.String(), `"error":`) != tc.refused || !strings.Contains(w.Body.String(), tc.want) {
			t.Fatal(w.Body.String())
		}
	}
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
			f.server.Handler().ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	f.clock.Advance(time.Hour)
	r := httptest.NewRequest("GET", "/api/status", nil)
	r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}, VerifiedChains: [][]*x509.Certificate{{leaf}}}
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
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
	f.server.Handler().ServeHTTP(w, r)
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
	}{{"streams", 64}, {"backlog", 10000}, {"wait", 600}} {
		for _, delta := range []int{0, 1} {
			b := fake.Bindings()
			r, err := api.Builtin(b)
			if err != nil {
				t.Fatal(err)
			}
			c := api.Config{FailureLog: slog.New(slog.NewTextHandler(io.Discard, nil)), Registry: r, Auth: fake.NewAuth(admin()), Runtime: fake.NewClock().Runtime(), Classify: publicerr.Classify}
			switch tc.key {
			case "streams":
				c.EventStreams = tc.limit + delta
			case "backlog":
				c.ReplayBacklog = tc.limit + delta
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
			f.server.Handler().ServeHTTP(w, r)
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

func TestSessionExactIdleBoundaryAndFullTableExpiry(t *testing.T) {
	for _, elapsed := range []time.Duration{time.Hour - time.Nanosecond, time.Hour} {
		f := setup(t, nil)
		assertSuccess(t, initialize(f))
		f.clock.Advance(elapsed)
		w := request(f, "DELETE", "/mcp", nil)
		want := 204
		if elapsed == time.Hour {
			want = 404
		}
		if w.Code != want {
			t.Fatalf("idle %s: %d", elapsed, w.Code)
		}
	}
	f := setup(t, nil)
	for i := 0; i < 64; i++ {
		assertSuccess(t, initialize(f))
	}
	f.clock.Advance(time.Hour)
	assertSuccess(t, initialize(f))
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

func TestProtocolHeadersAreValidatedOnEveryMCPBoundary(t *testing.T) {
	f := setup(t, nil)
	for _, path := range []string{"/mcp", "/mcp/enroll"} {
		for _, version := range []string{"2025-03-26", "2025-06-18", "invalid", "2030-01-01"} {
			r := httptest.NewRequest("POST", path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
			r.Header.Set("Content-Type", "application/json")
			if path == "/mcp" {
				r.Header.Set("Authorization", "Bearer valid")
			}
			r.Header.Set("MCP-Protocol-Version", version)
			w := httptest.NewRecorder()
			f.server.Handler().ServeHTTP(w, r)
			want := 200
			if version == "invalid" || version == "2030-01-01" {
				want = 400
			}
			if w.Code != want {
				t.Fatalf("%s version %s: %d", path, version, w.Code)
			}
		}
	}
	r := httptest.NewRequest("POST", "/mcp/enroll", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Add("MCP-Protocol-Version", apicontract.ProtocolVersion)
	r.Header.Add("MCP-Protocol-Version", apicontract.ProtocolVersion)
	w := httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	if w.Code != 400 {
		t.Fatal("duplicate protocol header accepted")
	}
	r = httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("MCP-Protocol-Version", "invalid")
	w = httptest.NewRecorder()
	f.server.Handler().ServeHTTP(w, r)
	if w.Code != 401 {
		t.Fatal("protocol negotiation replaced caller authentication")
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
