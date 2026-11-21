package githubfake

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

func newFake(t *testing.T) *Fake {
	t.Helper()
	return configuredFake(t, Config{})
}
func configuredFake(t *testing.T, config Config) *Fake {
	t.Helper()
	config.Root, config.HostKeys = t.TempDir(), []ssh.Signer{signer(t)}
	if config.Now == nil {
		config.Now = clock.RealClock{}.Now
	}
	f, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func signer(t *testing.T) ssh.Signer {
	t.Helper()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func seed(t *testing.T, f *Fake, s Seed) {
	t.Helper()
	if err := f.ApplySeed(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}
func state(t *testing.T, f *Fake) State {
	t.Helper()
	s, err := f.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return s
}

type testRequest struct {
	method, url string
	body        any
}

func request(t *testing.T, client *http.Client, value testRequest) (int, http.Header, []byte) {
	t.Helper()
	var in io.Reader
	if value.body != nil {
		data, err := json.Marshal(value.body)
		if err != nil {
			t.Fatal(err)
		}
		in = bytes.NewReader(data)
	}
	req, err := http.NewRequest(value.method, value.url, in)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer fixture")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	data, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, res.Header, data
}
func TestControlAndDeployKeyContract(t *testing.T) {
	f := newFake(t)
	control := httptest.NewServer(f.ControlHandler())
	defer control.Close()
	server := httptest.NewTLSServer(f)
	defer server.Close()
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer(t).PublicKey())))
	// Same JSON shape as the repository helper add-repo:204 and the command
	// fixture in internal/installer/plugin_add_repo_test.go:157. Body is ignored.
	status, _, body := request(t, server.Client(), testRequest{"POST", control.URL + "/_control/seed", Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}}})
	if status != 200 {
		t.Fatalf("seed: %d %s", status, body)
	}
	input := map[string]any{"title": "Oberth", "key": public, "read_only": false}
	status, _, body = request(t, server.Client(), testRequest{"POST", server.URL + "/api/v3/repos/ORG/OBERTH/keys", input})
	if status != 201 {
		t.Fatalf("create: %d %s", status, body)
	}
	var key DeployKey
	if err := json.Unmarshal(body, &key); err != nil {
		t.Fatal(err)
	}
	assertDeployKey(t, key, public)
	status, _, body = request(t, server.Client(), testRequest{"GET", control.URL + "/_control/state", nil})
	var s State
	if status != 200 || json.Unmarshal(body, &s) != nil || len(s.Repositories) != 1 || len(s.Repositories[0].Keys) != 1 || s.UnexpectedCount != 0 || len(s.HostKeys) == 0 || s.HostKeys[0].Fingerprint == "" {
		t.Fatalf("inspection: %d %s", status, body)
	}
}
func TestDeployKeyRefusals(t *testing.T) {
	f := newFake(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}})
	server := httptest.NewTLSServer(f)
	defer server.Close()
	public := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer(t).PublicKey())))
	for _, tc := range []struct {
		name, repository, key string
		status                int
	}{
		{"created", "org/oberth", public, 201}, {"duplicate", "org/oberth", public, 422},
		{"invalid", "org/oberth", "not a public key", 422}, {"missing", "org/missing", public, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := request(t, server.Client(), testRequest{"POST", server.URL + "/api/v3/repos/" + tc.repository + "/keys", map[string]any{"title": "Oberth", "key": tc.key, "read_only": false}})
			if status != tc.status {
				t.Fatalf("status %d, want %d", status, tc.status)
			}
		})
	}
	req, _ := http.NewRequest("POST", server.URL+"/api/v3/repos/org/oberth/keys", strings.NewReader(`{"key":"unused"}`))
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal(res.StatusCode)
	}
}

func TestHTTPInjectedErrors(t *testing.T) {
	for _, tc := range []struct {
		kind           string
		override, want int
		header, value  string
	}{
		{"rate_limit", 0, 403, "X-RateLimit-Remaining", "0"},
		{"rate_limit", 429, 429, "X-RateLimit-Reset", "4102444800"},
		{"secondary_rate_limit", 0, 403, "Retry-After", "60"},
		{"secondary_rate_limit", 429, 429, "Retry-After", "60"},
		{"not_found", 0, 404, "", ""}, {"forbidden", 0, 403, "", ""},
		{"server_error", 0, 503, "", ""}, {"server_error", 502, 502, "", ""},
	} {
		t.Run(tc.kind+strconv.Itoa(tc.override), func(t *testing.T) {
			f := newFake(t)
			control := httptest.NewServer(f.ControlHandler())
			defer control.Close()
			seed(t, f, Seed{Assets: []Asset{{Path: "/org/oberth/releases/download/v1/fixture", Bytes: []byte("approved bytes")}}})
			server := httptest.NewTLSServer(f)
			defer server.Close()
			target := "/org/oberth/releases/download/v1/fixture"
			status, _, body := request(t, server.Client(), testRequest{"POST", control.URL + "/_control/faults", Fault{Method: "GET", Path: target, Kind: tc.kind, Remaining: 1, Status: tc.override}})
			if status != 200 {
				t.Fatalf("inject: %d %s", status, body)
			}
			status, h, body := request(t, server.Client(), testRequest{"GET", server.URL + target, nil})
			if status != tc.want || tc.header != "" && h.Get(tc.header) != tc.value || !bytes.Contains(body, []byte("message")) {
				t.Fatalf("fault: %d %v %s", status, h, body)
			}
			status, _, body = request(t, server.Client(), testRequest{"GET", server.URL + target, nil})
			if status != 200 || string(body) != "approved bytes" {
				t.Fatalf("fault not consumed exactly once: %d %s", status, body)
			}
			s := state(t, f)
			if s.Faults[0].Remaining != 0 || s.UnexpectedCount != 0 {
				t.Fatal(s)
			}
		})
	}
}
func TestHTTPSlowAndDrop(t *testing.T) {
	for _, kind := range []string{"slow", "drop"} {
		t.Run(kind, func(t *testing.T) {
			f := newFake(t)
			path := "/org/oberth/releases/download/v1/fixture"
			seed(t, f, Seed{Assets: []Asset{{Path: path, Bytes: []byte("bytes")}}})
			server := httptest.NewTLSServer(f)
			defer server.Close()
			if err := f.Inject(Fault{Method: "GET", Path: path, Kind: kind, Remaining: 1, DelayMS: 150}); err != nil {
				t.Fatal(err)
			}
			transport := server.Client().Transport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, Timeout: 40 * time.Millisecond}
			res, err := client.Get(server.URL + path)
			if res != nil {
				res.Body.Close()
			}
			if err == nil {
				t.Fatal("injected transport error succeeded")
			}
			status, _, body := request(t, server.Client(), testRequest{"GET", server.URL + path, nil})
			if status != 200 || string(body) != "bytes" {
				t.Fatal(status, string(body))
			}
		})
	}
}
func TestUnknownAndInvalidRequests(t *testing.T) {
	f := newFake(t)
	control := httptest.NewServer(f.ControlHandler())
	defer control.Close()
	server := httptest.NewTLSServer(f)
	defer server.Close()
	for _, path := range []string{"/graphql", "/api/v3/repos/org/oberth/issues", "/api/v3/repos/org/oberth", "/api/v3/repos/org/oberth/releases", "/webhooks", "/org/oberth.git/info/refs?service=git-receive-pack", "/_control/unknown"} {
		status, _, body := request(t, server.Client(), testRequest{"GET", server.URL + path, nil})
		if status != 501 || !bytes.Contains(body, []byte("UNEXPECTED")) {
			t.Fatalf("%s: %d %s", path, status, body)
		}
	}
	if state(t, f).UnexpectedCount != 7 {
		t.Fatal(state(t, f))
	}
	for _, raw := range []string{`{"repositories":[{"name":"../escape"}]}`, `{"authorized_keys":["private material"]}`, `{"extra":true}`, `{} {}`, `{"assets":[{"path":"/../../escape"}]}`} {
		req, _ := http.NewRequest("POST", control.URL+"/_control/seed", strings.NewReader(raw))
		res, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != 400 {
			t.Fatalf("seed accepted %s: %d", raw, res.StatusCode)
		}
	}
	for _, v := range []Fault{{Method: "GET", Path: "*", Kind: "unknown", Remaining: 1}, {Method: "GET", Path: "*", Kind: "slow", Remaining: 1}, {Method: "GET", Path: "*", Kind: "drop"}, {Method: "GET", Path: "*", Kind: "server_error", Status: 200, Remaining: 1}, {Method: "GET", Path: "*", Kind: "archived", Remaining: 1}} {
		if f.Inject(v) == nil {
			t.Fatalf("accepted %+v", v)
		}
	}
	status, _, _ := request(t, server.Client(), testRequest{"GET", server.URL + "/org/oberth/releases/download/v1/absent", nil})
	if status != 404 {
		t.Fatal(status)
	}
}

func assertDeployKey(t *testing.T, key DeployKey, public string) {
	t.Helper()
	if key.ID != 1 || key.Key != public || key.Title != "Oberth" || key.ReadOnly || !key.Verified || key.CreatedAt != "2000-01-01T00:00:00Z" || key.URL != "/repos/org/oberth/keys/1" {
		t.Fatalf("wrong response: %+v", key)
	}
}

func TestFaultValidationRefusals(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault Fault
	}{
		{"attempts is read-only", Fault{Method: "GET", Path: "*", Kind: "forbidden", Remaining: 1, Attempts: 1}},
		{"positive remaining", Fault{Method: "GET", Path: "*", Kind: "drop"}},
		{"path is required", Fault{Method: "GET", Kind: "drop", Remaining: 1}},
		{"negative delay", Fault{Method: "GET", Path: "*", Kind: "slow", Remaining: 1, DelayMS: -1}},
		{"bounded delay", Fault{Method: "GET", Path: "*", Kind: "slow", Remaining: 1, DelayMS: 60001}},
		{"method", Fault{Method: "DELETE", Path: "*", Kind: "not_found", Remaining: 1}},
		{"kind", Fault{Method: "GET", Path: "*", Kind: "unknown", Remaining: 1}},
		{"push only", Fault{Method: "git-upload-pack", Path: "org/oberth", Kind: "protected", Remaining: 1}},
		{"HTTP throttle only", Fault{Method: "git-upload-pack", Path: "org/oberth", Kind: "rate_limit", Remaining: 1}},
		{"auth path", Fault{Method: "auth", Path: "org/oberth", Kind: "forbidden", Remaining: 1}},
		{"auth kind", Fault{Method: "auth", Path: "*", Kind: "server_error", Remaining: 1}},
		{"throttle status", Fault{Method: "GET", Path: "*", Kind: "rate_limit", Remaining: 1, Status: 500}},
		{"server status", Fault{Method: "GET", Path: "*", Kind: "server_error", Remaining: 1, Status: 499}},
		{"status does not apply", Fault{Method: "GET", Path: "*", Kind: "drop", Remaining: 1, Status: 403}},
		{"positive slow delay", Fault{Method: "GET", Path: "*", Kind: "slow", Remaining: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := newFake(t).Inject(tc.fault); err == nil {
				t.Fatal("invalid fault accepted")
			}
		})
	}
}
