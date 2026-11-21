package githubfake

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

type gitFixture struct {
	f       *Fake
	url     string
	env     []string
	work    string
	sign    ssh.Signer
	address string
}

func startSSH(t *testing.T, f *Fake) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.ServeSSH(ctx, listener) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(5 * time.Second):
			t.Error("SSH shutdown did not finish")
		}
	})
	return listener.Addr().String()
}
func gitSetup(t *testing.T) *gitFixture {
	t.Helper()
	return gitSetupWithConfig(t, Config{})
}
func gitSetupWithConfig(t *testing.T, config Config) *gitFixture {
	t.Helper()
	f := configuredFake(t, config)
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	sign, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}, AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(sign.PublicKey()))}})
	address := startSSH(t, f)
	_, port, _ := net.SplitHostPort(address)
	directory := t.TempDir()
	// Export only fixture private material, never real credentials.
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(directory, "identity")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	hosts := filepath.Join(directory, "known_hosts")
	if err := os.WriteFile(hosts, []byte(knownhosts.Line([]string{address}, f.signers[0].PublicKey())+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := sshCommand(t, keyFile, hosts)
	env := append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_SSH_COMMAND="+command, "GIT_SSH_VARIANT=ssh", "GHFAKE_TEST_KEY="+keyFile, "GHFAKE_TEST_HOSTS="+hosts, "GIT_AUTHOR_NAME=Test User", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test User", "GIT_COMMITTER_EMAIL=test@example.invalid")
	fixture := &gitFixture{f: f, url: "ssh://git@127.0.0.1:" + port + "/org/oberth.git", env: env, work: filepath.Join(directory, "work"), sign: sign, address: address}
	fixture.git(t, "", "init", "--initial-branch=main", fixture.work)
	if err := os.WriteFile(filepath.Join(fixture.work, "source.txt"), []byte("source delivered through Git\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fixture.git(t, fixture.work, "add", "source.txt")
	fixture.git(t, fixture.work, "commit", "-m", "Create source fixture")
	fixture.git(t, fixture.work, "remote", "add", "upstream", fixture.url)
	return fixture
}
func (g *gitFixture) command(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = g.env
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}
func (g *gitFixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := g.command(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return out
}

func TestGitTransportContract(t *testing.T) {
	// Matches promotion_tag_test.go:12, branch_deletion_test.go:183/:242 and
	// cache.go:501. Every source byte crosses a real Git service.
	g := gitSetup(t)
	sha := g.git(t, g.work, "rev-parse", "HEAD")
	g.git(t, g.work, "push", "upstream", sha+":refs/heads/main")
	advertised := g.git(t, g.work, "ls-remote", "--symref", "upstream", "HEAD")
	if !strings.Contains(advertised, "ref: refs/heads/main") || !strings.Contains(advertised, sha) {
		t.Fatal(advertised)
	}
	testGitObjects(t, g, sha)
	testGitRefConflicts(t, g, sha)
}
func testGitObjects(t *testing.T, g *gitFixture, sha string) {
	t.Helper()
	clone := filepath.Join(t.TempDir(), "clone")
	g.git(t, "", "clone", "--depth", "1", g.url, clone)
	body, err := os.ReadFile(filepath.Join(clone, "source.txt"))
	if err != nil || string(body) != "source delivered through Git\n" {
		t.Fatalf("source missing: %q %v", body, err)
	}
	g.git(t, g.work, "tag", "-a", "v1.0.0", "-m", "Source fixture")
	object := g.git(t, g.work, "rev-parse", "refs/tags/v1.0.0")
	if object == sha {
		t.Fatal("annotated object lost")
	}
	g.git(t, g.work, "push", "upstream", object+":refs/tags/v1.0.0")
	refs := g.git(t, g.work, "ls-remote", "--tags", "upstream")
	if !strings.Contains(refs, object) || !strings.Contains(refs, sha) {
		t.Fatal(refs)
	}
	out, err := g.command(g.work, "push", "upstream", sha+":refs/tags/v1.0.0")
	if err == nil || !strings.Contains(out, "already exists") {
		t.Fatalf("immutable tag conflict: %v %s", err, out)
	}
	g.git(t, g.work, "push", "upstream", ":refs/tags/v1.0.0")
	if refs := g.git(t, g.work, "ls-remote", "--tags", "upstream"); refs != "" {
		t.Fatalf("tag not deleted: %s", refs)
	}
	g.git(t, g.work, "push", "upstream", object+":refs/tags/v1.0.0")
	g.git(t, g.work, "fetch", "--prune", "--no-tags", "upstream", "+refs/heads/*:refs/oberth/upstream/heads/*", "+refs/tags/*:refs/oberth/upstream/tags/*")
	if got := g.git(t, g.work, "rev-parse", "refs/oberth/upstream/tags/v1.0.0"); got != object {
		t.Fatal(got)
	}
	if got := state(t, g.f).Repositories[0].Refs["refs/tags/v1.0.0"]; got != object {
		t.Fatal(got)
	}
}
func testGitRefConflicts(t *testing.T, g *gitFixture, sha string) {
	t.Helper()
	g.git(t, g.work, "push", "upstream", sha+":refs/heads/topic")
	wrong := strings.Repeat("0", 40)
	out, err := g.command(g.work, "push", "--force-with-lease=refs/heads/topic:"+wrong, "upstream", ":refs/heads/topic")
	if err == nil || !strings.Contains(out, "stale info") {
		t.Fatalf("stale lease: %v %s", err, out)
	}
	g.git(t, g.work, "push", "--force-with-lease=refs/heads/topic:"+sha, "upstream", ":refs/heads/topic")
	if got := g.git(t, g.work, "ls-remote", "--heads", "upstream", "refs/heads/topic"); got != "" {
		t.Fatal(got)
	}
	// Real server refusal of non-fast-forward; --force explicitly succeeds.
	if err := os.WriteFile(filepath.Join(g.work, "source.txt"), []byte("second revision\n"), 0600); err != nil {
		t.Fatal(err)
	}
	g.git(t, g.work, "commit", "-am", "Advance source fixture")
	g.git(t, g.work, "push", "upstream", "HEAD:refs/heads/main")
	out, err = g.command(g.work, "push", "upstream", sha+":refs/heads/main")
	if err == nil || !strings.Contains(out, "non-fast-forward") {
		t.Fatalf("non-fast-forward: %v %s", err, out)
	}
	g.git(t, g.work, "push", "--force", "upstream", sha+":refs/heads/main")
	s := state(t, g.f)
	if s.UnexpectedCount != 0 || s.Repositories[0].Refs["refs/heads/main"] != sha {
		t.Fatal(s)
	}
}
func TestSmartHTTPAndDownloadContract(t *testing.T) {
	g := gitSetup(t)
	g.git(t, g.work, "push", "upstream", "HEAD:refs/heads/main")
	server := httptest.NewTLSServer(g.f)
	defer server.Close()
	caFile := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}), 0644); err != nil {
		t.Fatal(err)
	}
	g.env = append(g.env, "GIT_SSL_CAINFO="+caFile)
	clone := filepath.Join(t.TempDir(), "http-clone")
	g.git(t, "", "clone", "--depth", "1", server.URL+"/org/oberth.git", clone)
	if got := g.git(t, clone, "show", "HEAD:source.txt"); got != "source delivered through Git" {
		t.Fatal(got)
	}
	seed(t, g.f, Seed{Assets: []Asset{{Path: "/org/oberth/releases/download/v1/fixture.bin", Bytes: []byte{0, 1, 2, 255}}}})
	status, headers, body := request(t, server.Client(), testRequest{"GET", server.URL + "/org/oberth/releases/download/v1/fixture.bin", nil})
	if status != 200 || headers.Get("Content-Type") != "application/octet-stream" || !bytes.Equal(body, []byte{0, 1, 2, 255}) {
		t.Fatal(status, headers, body)
	}
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal(state(t, g.f))
	}
}
func dial(t *testing.T, g *gitFixture, key ssh.Signer, host ssh.PublicKey) (*ssh.Client, error) {
	t.Helper()
	return ssh.Dial("tcp", g.address, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host), Timeout: time.Second})
}

func sshCommand(t *testing.T, key, hosts string) string {
	t.Helper()
	binary, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ssh_config")
	config := fmt.Sprintf("Host *\n IdentityFile %q\n UserKnownHostsFile %q\n GlobalKnownHostsFile /dev/null\n StrictHostKeyChecking yes\n IdentitiesOnly yes\n BatchMode yes\n SendEnv LANG LC_*\n", key, hosts)
	if err := os.WriteFile(path, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	return shellArg(binary) + " -F " + shellArg(path)
}
func shellArg(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }
