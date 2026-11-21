package githubfake

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func trustServer(t *testing.T, f *Fake) (*httptest.Server, string) {
	t.Helper()
	cfg, ca, err := ServerTLS(TLSOptions{Now: f.now()})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(f)
	server.TLS = cfg
	server.StartTLS()
	t.Cleanup(server.Close)
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, ca, 0644); err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	server.Client().Transport = &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots}}
	return server, caFile
}
func TestCurlHTTPSRetryContract(t *testing.T) {
	binary, err := exec.LookPath("curl")
	if err != nil {
		t.Skip("curl unavailable; pinned TLS transport is covered by Go clients")
	}
	f := newFake(t)
	target := "/org/oberth/releases/download/v1/fixture"
	seed(t, f, Seed{Assets: []Asset{{Path: target, Bytes: []byte("approved bytes")}}})
	server, ca := trustServer(t, f)
	// Exact transport/retry flags from .oberth/build.yaml:706-724, :3079-3083
	// and .oberth/release.yaml:748-767, with only local trust/output added.
	for _, remaining := range []int{1, 3} {
		if err := f.Inject(Fault{Method: "GET", Path: target, Kind: "server_error", Remaining: remaining}); err != nil {
			t.Fatal(err)
		}
		out := filepath.Join(t.TempDir(), "download")
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		args := []string{"--fail", "--location", "--no-progress-meter", "--show-error", "--connect-timeout", "10", "--max-time", "90", "--retry", "2", "--retry-all-errors", "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "--cacert", ca, "--output", out, server.URL + target}
		_, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
		cancel()
		if remaining == 1 {
			if err != nil {
				t.Fatal("client did not recover after one fault")
			}
			body, err := os.ReadFile(out)
			if err != nil || string(body) != "approved bytes" {
				t.Fatal("download bytes differ")
			}
		} else if err == nil {
			t.Fatal("three consecutive faults were hidden by retries")
		}
		assertLastFault(t, f, remaining)
	}
	if got := state(t, f); got.UnexpectedCount != 0 || len(got.Calls) != 5 {
		t.Fatal("retry request accounting differs")
	}
}
func TestCurlHTTPSRedirectAndTLSContract(t *testing.T) {
	binary, err := exec.LookPath("curl")
	if err != nil {
		t.Fatal(err)
	}
	f := newFake(t)
	target := "/org/oberth/releases/download/v1/fixture"
	seed(t, f, Seed{Assets: []Asset{{Path: target, Bytes: []byte("approved bytes")}}})
	server, ca := trustServer(t, f)
	var plainHits atomic.Int32
	plain := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { plainHits.Add(1) }))
	defer plain.Close()
	var destination atomic.Value
	destination.Store(server.URL + target)
	redirect := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.Load().(string), http.StatusFound)
	}))
	defer redirect.Close()
	bundle, _ := os.ReadFile(ca)
	bundle = append(bundle, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: redirect.Certificate().Raw})...)
	if err := os.WriteFile(ca, bundle, 0644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--fail", "--location", "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "--cacert", ca, redirect.URL)
	if out, err := cmd.Output(); err != nil || string(out) != "approved bytes" {
		t.Fatal("HTTPS redirect was not followed")
	}
	destination.Store(plain.URL)
	cmd = exec.Command(binary, "--fail", "--location", "--proto", "=https", "--proto-redir", "=https", "--tlsv1.2", "--cacert", ca, redirect.URL)
	if cmd.Run() == nil || plainHits.Load() != 0 {
		t.Fatal("HTTPS-only redirect policy was bypassed")
	}
	cfg := server.Client().Transport.(*http.Transport).Clone()
	cfg.TLSClientConfig.MinVersion = tls.VersionTLS10
	cfg.TLSClientConfig.MaxVersion = tls.VersionTLS11
	defer cfg.CloseIdleConnections()
	if res, err := (&http.Client{Transport: cfg}).Get(server.URL + target); err == nil {
		res.Body.Close()
		t.Fatal("TLS below 1.2 accepted")
	}
	if got := state(t, f); got.UnexpectedCount != 0 || len(got.Calls) != 1 {
		t.Fatalf("unexpected HTTPS attempt count: %d", len(got.Calls))
	}
}
func TestHelperAuthenticationAndEnterprisePrefix(t *testing.T) {
	f := newFake(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}})
	server, _ := trustServer(t, f)
	status, headers, _ := request(t, server.Client(), testRequest{"GET", server.URL + "/api/v3/", nil})
	if status != 200 || headers.Get("X-Oauth-Scopes") != "repo, read:org, gist" {
		t.Fatal(status, headers)
	}
	status, _, _ = request(t, server.Client(), testRequest{"POST", server.URL + "/api/v3/repos/ORG/OBERTH/keys", map[string]any{"title": "Oberth", "key": string(ssh.MarshalAuthorizedKey(signer(t).PublicKey())), "read_only": false}})
	if status != 201 {
		t.Fatal(status)
	}
	if state(t, f).UnexpectedCount != 0 {
		t.Fatal("helper exchange counted as unexpected")
	}
	status, _, _ = request(t, server.Client(), testRequest{"POST", server.URL + "/api/graphql", map[string]any{"query": "query Other{anything}"}})
	if status != 501 || state(t, f).UnexpectedCount != 1 {
		t.Fatal("unlisted query accepted")
	}
	req, _ := http.NewRequest("GET", server.URL+"/api/v3/", nil)
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 401 {
		t.Fatal("unauthenticated probe accepted")
	}
}

type httpRoute struct {
	method, path, content, body string
	success                     int
}

func TestFaultEveryHTTPPath(t *testing.T) {
	public := string(ssh.MarshalAuthorizedKey(signer(t).PublicKey()))
	routes := []httpRoute{
		{"POST", "/api/v3/repos/org/oberth/keys", "application/json", `{"title":"Oberth","key":` + strconv.Quote(public) + `,"read_only":false}`, 201},
		{"GET", "/org/oberth.git/info/refs?service=git-upload-pack", "", "", 200},
		{"POST", "/org/oberth.git/git-upload-pack", "Application/X-Git-Upload-Pack-Request", "0000", 200},
		{"GET", "/org/oberth/releases/download/v1/fixture", "", "", 200},
		{"GET", "/api/v3/", "", "", 200},
	}
	for i, route := range routes {
		for _, kind := range []string{"rate_limit", "secondary_rate_limit", "not_found", "forbidden", "server_error", "slow", "drop"} {
			t.Run(fmt.Sprintf("%d/%s", i, kind), func(t *testing.T) { testHTTPFault(t, route, kind) })
		}
	}
}
func testHTTPFault(t *testing.T, route httpRoute, kind string) {
	t.Helper()
	gate := newDelayGate()
	f := configuredFake(t, Config{Delay: gate.wait})
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}, Assets: []Asset{{Path: "/org/oberth/releases/download/v1/fixture", Bytes: []byte("bytes")}}})
	server := httptest.NewTLSServer(f)
	defer server.Close()
	defer gate.open()
	if err := f.Inject(Fault{Method: route.method, Path: strings.Split(route.path, "?")[0], Kind: kind, Remaining: 1, DelayMS: 20}); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(route.method, server.URL+route.path, strings.NewReader(route.body))
	req.Header.Set("Authorization", "token fixture")
	req.Header.Set("Content-Type", route.content)
	var wait *delayGate
	if kind == "slow" {
		wait = gate
	}
	result := performHTTP(t, server.Client(), req, wait)
	want := map[string]int{"rate_limit": 403, "secondary_rate_limit": 403, "not_found": 404, "forbidden": 403, "server_error": 503, "slow": route.success}[kind]
	assertHTTPFaultResponse(t, result, want)
	assertLastFault(t, f, 1)
	if state(t, f).UnexpectedCount != 0 {
		t.Fatal("incorrect accounting")
	}
}

type httpResult struct {
	status int
	err    error
}

func performHTTP(t *testing.T, client *http.Client, req *http.Request, gate *delayGate) httpResult {
	t.Helper()
	client.Timeout = 3 * time.Second
	done := make(chan httpResult, 1)
	go func() { done <- readHTTPResponse(client, req) }()
	if gate != nil {
		gate.await(t, 20*time.Millisecond)
		select {
		case result := <-done:
			t.Fatalf("HTTP completed before delay release: %v", result.err)
		default:
		}
		gate.open()
	}
	return <-done
}
func assertHTTPFaultResponse(t *testing.T, result httpResult, want int) {
	t.Helper()
	if want == 0 {
		if result.err == nil {
			t.Fatal("drop returned a response")
		}
		return
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	if result.status != want {
		t.Fatalf("status %d, want %d", result.status, want)
	}
}
func TestCaseMatchingAndReceivePackRefusal(t *testing.T) {
	f := newFake(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}, Assets: []Asset{{Path: "/ORG/OBERTH/releases/download/V1/Fixture", Bytes: []byte("bytes")}}})
	server := httptest.NewTLSServer(f)
	defer server.Close()
	if err := f.Inject(Fault{Method: "POST", Path: "/api/v3/repos/org/oberth/keys", Kind: "not_found", Remaining: 1}); err != nil {
		t.Fatal(err)
	}
	status, _, _ := request(t, server.Client(), testRequest{"POST", server.URL + "/api/v3/repos/ORG/OBERTH/keys", nil})
	if status != 404 || state(t, f).Faults[0].Attempts != 1 {
		t.Fatal("repository case changed fault matching")
	}
	for _, tc := range []struct {
		path string
		want int
	}{{"/org/oberth/releases/download/V1/Fixture", 200}, {"/org/oberth/releases/download/v1/Fixture", 404}} {
		status, _, _ := request(t, server.Client(), testRequest{"GET", server.URL + tc.path, nil})
		if status != tc.want {
			t.Fatal(status)
		}
	}
	req, _ := http.NewRequest("POST", server.URL+"/org/oberth.git/git-receive-pack", strings.NewReader("0000"))
	req.Header.Set("Content-Type", "application/x-git-receive-pack-request")
	res, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 501 || state(t, f).UnexpectedCount != 1 {
		t.Fatal("HTTP receive-pack accepted")
	}
	if err := f.Inject(Fault{Method: "GET", Path: "*", Kind: "not_found", Remaining: 1}); err != nil {
		t.Fatal(err)
	}
	status, _, _ = request(t, server.Client(), testRequest{"GET", server.URL + "/org/oberth/releases/download/V1/Fixture", nil})
	if status != 404 || state(t, f).Faults[1].Attempts != 1 {
		t.Fatal("wildcard did not match")
	}
}

func assertLastFault(t *testing.T, f *Fake, attempts int) {
	t.Helper()
	faults := state(t, f).Faults
	last := faults[len(faults)-1]
	if last.Attempts != attempts || last.Remaining != 0 {
		t.Fatal("fault attempts not observable")
	}
}
func TestUninventoriedRoutesAreRefused(t *testing.T) {
	f := newFake(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}})
	server := httptest.NewTLSServer(f)
	defer server.Close()
	routes := []struct{ method, path string }{{"GET", "/"}, {"POST", "/repos/org/oberth/keys"}, {"POST", "/graphql"}, {"POST", "/api/graphql"}}
	for _, route := range routes {
		status, _, _ := request(t, server.Client(), testRequest{route.method, server.URL + route.path, map[string]string{"query": "query UserCurrent{viewer{login}}"}})
		if status != 501 {
			t.Fatalf("%s %s: %d", route.method, route.path, status)
		}
	}
	if state(t, f).UnexpectedCount != len(routes) {
		t.Fatal("unlisted routes not counted")
	}
}

func readHTTPResponse(client *http.Client, req *http.Request) httpResult {
	res, err := client.Do(req)
	if err != nil {
		return httpResult{err: err}
	}
	defer res.Body.Close()
	_, err = io.Copy(io.Discard, res.Body)
	return httpResult{status: res.StatusCode, err: err}
}
