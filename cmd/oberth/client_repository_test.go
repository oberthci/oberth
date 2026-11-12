package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/clientcert"
	"github.com/oberthci/oberth/internal/installer"
)

func bindingWrite(t *testing.T, path string, body []byte) string {
	t.Helper()
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
func bindingGit(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	for _, item := range os.Environ() {
		if item != "GIT_SSH_COMMAND=" && item != "GIT_SSH=" {
			cmd.Env = append(cmd.Env, item)
		}
	}
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("fixture git: %v %s", err, raw)
	}
	return strings.TrimSpace(string(raw))
}

func bindingSSHServer(t *testing.T, signer ssh.Signer, clientKey ssh.PublicKey, bare string) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	calls := &atomic.Int32{}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if bytes.Equal(key.Marshal(), clientKey.Marshal()) {
			return nil, nil
		}
		return nil, fmt.Errorf("unexpected identity")
	}}
	config.AddHostKey(signer)
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				server, channels, requests, err := ssh.NewServerConn(conn, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					if incoming.ChannelType() != "session" {
						_ = incoming.Reject(ssh.UnknownChannelType, "session only")
						continue
					}
					ch, requests, err := incoming.Accept()
					if err != nil {
						continue
					}
					go func() {
						defer func() { _ = ch.Close() }()
						for request := range requests {
							if request.Type != "exec" {
								_ = request.Reply(false, nil)
								continue
							}
							var command struct{ Command string }
							if ssh.Unmarshal(request.Payload, &command) != nil || !strings.HasPrefix(command.Command, "git-receive-pack ") || !strings.Contains(command.Command, "github/acme/project.git") {
								_ = request.Reply(false, nil)
								return
							}
							_ = request.Reply(true, nil)
							calls.Add(1)
							cmd := exec.Command("git-receive-pack", bare)
							cmd.Stdin = ch
							cmd.Stdout = ch
							cmd.Stderr = ch.Stderr()
							status := uint32(0)
							if cmd.Run() != nil {
								status = 1
							}
							_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
							return
						}
					}()
				}
			}()
		}
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	return "ssh://git@localhost:" + port, calls
}

func bindingFixture(t *testing.T, root, keyFile, gitURL string, sshSigner ssh.Signer) repositoryClientOptions {
	t.Helper()
	clientFiles, err := clientcert.GenerateAuthority()
	if err != nil {
		t.Fatal(err)
	}
	authority, err := clientcert.Load(clientFiles["ca.crt"], clientFiles["ca.key"])
	if err != nil {
		t.Fatal(err)
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		t.Fatal(err)
	}
	cert, _, err := authority.Issue("agent", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csr}), 0)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	identity, serverCA := inviteCLIIdentity(t)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.VerifiedChains) == 0 || r.Header.Get("Authorization") != "" {
			t.Error("registration lookup lacked certificate-only authentication")
		}
		var request jsonRPCRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			w.WriteHeader(400)
			return
		}
		raw, _ := json.Marshal(request.Params)
		var params toolsCallParams
		_ = json.Unmarshal(raw, &params)
		var result any
		switch params.Name {
		case "system_status":
			result = map[string]string{"ssh_host_key": strings.TrimSpace(string(ssh.MarshalAuthorizedKey(sshSigner.PublicKey()))), "ssh_host_key_fingerprint": ssh.FingerprintSHA256(sshSigner.PublicKey())}
		case "repo_list":
			result = map[string]any{"repositories": []map[string]string{{"git_path": "github/acme/project"}}}
		default:
			t.Error(params.Name)
			w.WriteHeader(400)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": map[string]any{"structuredContent": result}})
	})
	server := httptest.NewUnstartedServer(handler)
	pool := x509.NewCertPool()
	pool.AddCert(authority.Certificate)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{identity}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	t.Cleanup(server.Close)
	base := strings.Replace(server.URL, "127.0.0.1", "localhost", 1)
	pin := sha256.Sum256(serverCA.Raw)
	install := installer.InstallResult{Version: 1, ServerURL: base, GitURL: gitURL, CAPin: hex.EncodeToString(pin[:])}
	body, _ := json.Marshal(install)
	files := t.TempDir()
	return repositoryClientOptions{Path: root, Installation: bindingWrite(t, filepath.Join(files, "install.json"), body), RegisteredRepo: "github/acme/project", SSHKey: keyFile, CAFile: bindingWrite(t, filepath.Join(files, "ca.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverCA.Raw})), ClientCert: bindingWrite(t, filepath.Join(files, "client.crt"), cert), ClientKey: bindingWrite(t, filepath.Join(files, "client.key"), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), Mode: "remote", Consent: true}
}

func TestClientRepositoryTwoServersPushOnlyToTheirPinnedServer(t *testing.T) {
	t.Setenv("OBERTH_MCP_URL", "https://another-server.example.test/mcp")
	t.Setenv("OBERTH_TOKEN", "unused-ambient-canary")
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("OpenSSH unavailable")
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "config with spaces"))
	_, clientKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSigner, err := ssh.NewSignerFromKey(clientKey)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(clientKey, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := bindingWrite(t, filepath.Join(t.TempDir(), "push key"), pem.EncodeToMemory(block))
	configs := []string{}
	calls := []*atomic.Int32{}
	bares := []string{}
	roots := []string{}
	urls := []string{}
	for i := range 2 {
		root := t.TempDir()
		bare := t.TempDir()
		bindingGit(t, bare, "init", "--bare")
		bindingGit(t, bare, "config", "receive.advertisePushOptions", "true")
		bindingGit(t, root, "init", "-b", "main")
		bindingWrite(t, filepath.Join(root, "README"), []byte(fmt.Sprintf("server %d\n", i)))
		bindingGit(t, root, "add", "README")
		bindingGit(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "-m", "fixture")
		_, hostPrivate, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
		if err != nil {
			t.Fatal(err)
		}
		gitURL, count := bindingSSHServer(t, hostSigner, clientSigner.PublicKey(), bare)
		o := bindingFixture(t, root, keyFile, gitURL, hostSigner)
		o.Assist = i == 0
		preview := o
		preview.Consent = false
		binding, err := configureClientRepository(t.Context(), preview)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = os.Stat(binding.SSHConfig); !os.IsNotExist(err) {
			t.Fatal("preview wrote SSH files")
		}
		binding, err = configureClientRepository(t.Context(), o)
		if err != nil {
			t.Fatal(err)
		}
		configs = append(configs, binding.SSHConfig)
		calls = append(calls, count)
		bares = append(bares, bare)
		roots = append(roots, root)
		urls = append(urls, binding.PushURL)
		if binding.PushURL != gitURL+"/github/acme/project.git" || !binding.Configured {
			t.Fatal(binding)
		}
		if i == 1 {
			_, err := configureClientRepository(t.Context(), o)
			if err != nil {
				t.Fatal("idempotent binding failed", err)
			}
		}
		bindingGit(t, root, "push", "oberth", "HEAD:refs/heads/accepted")
		if count.Load() != 1 {
			t.Fatal("push did not use its server")
		}
	}
	// Even changing the URL to the other server on the same DNS name
	// cannot cross the pinned host identity in this repository's SSH file.
	bindingGit(t, roots[0], "config", "--local", "remote.oberth.url", urls[1])
	wrongPush := exec.Command("git", "-C", roots[0], "push", "oberth", "HEAD:refs/heads/wrong-server")
	for _, item := range os.Environ() {
		if item != "GIT_SSH_COMMAND=" && item != "GIT_SSH=" {
			wrongPush.Env = append(wrongPush.Env, item)
		}
	}
	if _, err := wrongPush.CombinedOutput(); err == nil {
		t.Fatal("host-key binding allowed a push to the other server")
	}
	if configs[0] == configs[1] {
		t.Fatal("servers share a mutable SSH configuration")
	}
	for i := range 2 {
		if calls[i].Load() != 1 || bindingGit(t, bares[i], "rev-parse", "refs/heads/accepted") == "" {
			t.Fatal("wrong server received a push")
		}
	}
}

func TestClientRepositoryRefusesWrongPinMissingRegistrationAndSSHOverride(t *testing.T) {
	t.Setenv("GIT_SSH_COMMAND", "")
	t.Setenv("GIT_SSH", "")
	configRoot := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", configRoot)
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(t.TempDir(), "global"))
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root := t.TempDir()
	bindingGit(t, root, "init")
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	sshFile := bindingWrite(t, filepath.Join(t.TempDir(), "push"), []byte("local identity fixture"))
	o := bindingFixture(t, root, sshFile, "ssh://git@localhost:32022", signer)
	wrong := o
	wrong.RegisteredRepo = "github/acme/missing"
	if _, err := configureClientRepository(t.Context(), wrong); err == nil {
		t.Fatal("accepted unregistered route")
	}
	wrong = o
	wrong.CAFile = bindingWrite(t, filepath.Join(t.TempDir(), "other.crt"), []byte("not the selected CA"))
	if _, err := configureClientRepository(t.Context(), wrong); err == nil {
		t.Fatal("accepted another CA")
	}
	bindingGit(t, root, "config", "--local", "url.ssh://git@elsewhere/.insteadOf", "ssh://git@localhost:")
	if _, err := configureClientRepository(t.Context(), o); err == nil {
		t.Fatal("accepted a URL rewrite to another server")
	}
	bindingGit(t, root, "config", "--local", "--unset-all", "url.ssh://git@elsewhere/.insteadOf")
	t.Setenv("GIT_SSH_COMMAND", "ssh -F another-server")
	if _, err := configureClientRepository(t.Context(), o); err == nil {
		t.Fatal("accepted an ambient SSH override")
	}
	t.Setenv("GIT_SSH_COMMAND", "")
	_, bindingErr := os.Stat(filepath.Join(configRoot, "oberth"))
	if !os.IsNotExist(bindingErr) {
		t.Fatal("refused binding wrote configuration")
	}
}
