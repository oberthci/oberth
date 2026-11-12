package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/app"
	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/service"
	"github.com/oberthci/oberth/internal/store"
	"github.com/oberthci/oberth/internal/storetest"
)

func TestNoWaitPendingGitHubIsReadyWithCertificateMCP(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "state.sqlite")
	database, err := storetest.Open(ctx, path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	hostKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"))
	if err != nil {
		t.Fatal(err)
	}
	privatePath, hostsPath := filepath.Join(dir, "id_ed25519"), filepath.Join(dir, "known_hosts")
	probes := 0
	dependencies := upstreamDependencies{
		input: strings.NewReader(""), mutationGate: allowTestMutation,
		identityStore: fixtureStoreFactory(func() (kubernetes.Interface, error) { return client, nil }),
		scanHostKeys:  func(context.Context, string, string) ([]ssh.PublicKey, error) { return []ssh.PublicKey{hostKey}, nil },
		probe: func(context.Context, string, []byte, []byte) error {
			probes++
			return errors.New("ssh: unable to authenticate")
		},
		repositoryProbe: func(context.Context, string, string, []byte, []byte) error {
			t.Fatal("no-wait probed a repository")
			return nil
		},
	}
	args := []string{"add", "--yes", "--no-wait", "--database", path, "--upstream-key", privatePath, "--known-hosts", hostsPath, "--namespace", "ci", "github", "ssh://git@github.com/acme"}
	// Generated and recovered identities must both skip authentication.
	for range 2 {
		var output bytes.Buffer
		if err := runUpstreamWithDependencies(ctx, args, &output, dependencies); err != nil {
			t.Fatal(err)
		}
		if probes != 0 || !strings.Contains(output.String(), "registered; deploy key pending.") || !strings.Contains(output.String(), "ssh-ed25519 ") || strings.Contains(output.String(), "PRIVATE KEY") {
			t.Fatal("no-wait probed, failed to register pending, or disclosed private material")
		}
	}
	identity, err := client.CoreV1().Secrets("ci").Get(ctx, "oberth-upstream-key", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	hosts, err := client.CoreV1().Secrets("ci").Get(ctx, "oberth-known-hosts", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Model the identity projector making persisted material available to serve.
	if err := os.WriteFile(privatePath, identity.Data["id_ed25519"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, hosts.Data["known_hosts"], 0600); err != nil {
		t.Fatal(err)
	}
	if err := runUpstreamWithDependencies(ctx, args, io.Discard, dependencies); err != nil || probes != 0 {
		t.Fatal("projected no-wait identity performed authentication")
	}
	upstreams, err := database.ListUpstreams(ctx)
	if err != nil || len(upstreams) != 1 || upstreams[0].Name != "github" {
		t.Fatal("pending registration is not durable and idempotent")
	}
	health := app.Health{Store: database, Configured: upstreamReadinessProbe(serveOptions{upstreamKey: privatePath, knownHosts: hostsPath}), VCS: func(context.Context, model.Upstream) error { return errors.New("forge key not accepted") }}
	if err := health.Ready(ctx); err != nil {
		t.Fatalf("pending registration not ready: %v", err)
	}

	files, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	ca, err := clientcert.Load(files["ca.crt"], files["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	uplink, err := database.RegisterCertificateUplink(ctx, "install", model.UplinkSpec{Identity: "admin@workstation", Fingerprint: "SHA256:install-admin", Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, private)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := service.IssueClientCert(ctx, &service.ClientCertIssuer{Authority: ca, Store: database}, "install", uplink.Identity, uplink.Fingerprint, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair([]byte(cert), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatal(err)
	}
	backend, err := service.NewAPI(service.APIConfig{Runs: database, Health: health})
	if err != nil {
		t.Fatal(err)
	}
	apiServer, err := api.New(&service.Authenticator{Certificates: database}, backend, backend, "test")
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.Certificate)
	server := httptest.NewUnstartedServer(apiServer.Handler())
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	transport := &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, Certificates: []tls.Certificate{pair}}}
	defer transport.CloseIdleConnections()
	httpClient := &http.Client{Transport: transport}
	readyRequest, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/readyz", nil)
	if err != nil {
		t.Fatal(err)
	}
	readyResponse, err := httpClient.Do(readyRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = readyResponse.Body.Close()
	if readyResponse.StatusCode != http.StatusOK {
		t.Fatal("pending upstream excluded the server from Service readiness")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"system_status","arguments":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := httpClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var reply struct {
		Error  json.RawMessage `json:"error"`
		Result struct {
			IsError bool              `json:"isError"`
			Content []json.RawMessage `json:"content"`
		} `json:"result"`
	}
	if json.NewDecoder(response.Body).Decode(&reply) != nil || response.StatusCode != 200 || len(reply.Error) != 0 || reply.Result.IsError || len(reply.Result.Content) == 0 {
		t.Fatal("certificate-only MCP failed while forge authorization was pending")
	}
}

func TestNoWaitStillRefusesUntrustedForgeHostKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.sqlite")
	if err := storetest.EnsureDatabase(t.Context(), path, store.Options{}); err != nil {
		t.Fatal(err)
	}
	client := newBootstrapClient("ci", "oberth-upstream-key", "oberth-known-hosts")
	err := runUpstreamWithDependencies(t.Context(), []string{"add", "--yes", "--no-wait", "--database", path, "--upstream-key", filepath.Join(dir, "key"), "--known-hosts", filepath.Join(dir, "hosts"), "--namespace", "ci", "github", "ssh://git@github.com/acme"}, io.Discard, upstreamDependencies{
		input: strings.NewReader(""), mutationGate: allowTestMutation, identityStore: fixtureStoreFactory(func() (kubernetes.Interface, error) { return client, nil }),
		scanHostKeys: func(context.Context, string, string) ([]ssh.PublicKey, error) {
			return []ssh.PublicKey{codebergSSHHostKey(t)}, nil
		},
		probe: func(context.Context, string, []byte, []byte) error {
			t.Fatal("auth probe before pin validation")
			return nil
		},
	})
	if err == nil || upstreamCount(t, path) != 0 {
		t.Fatal("no-wait bypassed host pin validation")
	}
}

func TestInstallPendingExitCode(t *testing.T) {
	if cliFailureExitCode(installer.ErrUpstreamKeyPending) != 3 || cliFailureExitCode(installer.ErrOnboardingPartial) != 1 || cliFailureExitCode(errors.New("install failed")) != 1 || cliFailureExitCode(errUsage) != 2 {
		t.Fatal("incorrect command exit classification")
	}
}

func TestUpstreamCheckReportsPendingUntilForgeAcceptsKey(t *testing.T) {
	hostPublic, hostPrivate, _ := testSSHIdentity(t)
	_, private, _ := testSSHIdentity(t)
	signer, err := ssh.ParsePrivateKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	var authorized atomic.Bool
	config := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) {
		if authorized.Load() {
			return nil, nil
		}
		return nil, errors.New("key not registered")
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			raw, err := listener.Accept()
			if err != nil {
				return
			}
			conn, _, requests, err := ssh.NewServerConn(raw, config)
			if err != nil {
				_ = raw.Close()
				continue
			}
			ssh.DiscardRequests(requests)
			_ = conn.Close()
		}
	}()
	dir := t.TempDir()
	keyPath, hostsPath := filepath.Join(dir, "key"), filepath.Join(dir, "hosts")
	if err := os.WriteFile(keyPath, private, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(hostsPath, []byte(knownhosts.Line([]string{listener.Addr().String()}, hostPublic)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	upstream := model.Upstream{Kind: "ssh", BaseURL: "ssh://git@" + listener.Addr().String() + "/acme"}
	if upstreamAuthStatus(t.Context(), upstream, "", keyPath, dir, hostsPath) != "pending: the forge has not accepted this deploy key" {
		t.Fatal("auth rejection did not report pending")
	}
	authorized.Store(true)
	if upstreamAuthStatus(t.Context(), upstream, "", keyPath, dir, hostsPath) != "ok" {
		t.Fatal("authorized key did not report verified authentication")
	}
}
