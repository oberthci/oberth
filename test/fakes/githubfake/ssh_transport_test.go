package githubfake

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"k8s.io/utils/clock"
)

func TestOpenSSHCleanExchange(t *testing.T) {
	g := gitSetup(t)
	g.env = append(g.env, "LANG=C.UTF-8", "LC_OBERTH_TEST=C")
	g.git(t, g.work, "ls-remote", "upstream")
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal("clean OpenSSH exchange counted as unexpected")
	}
}

// ref-old/internal/pushretry/retry.go:48 and :63 classify these substrings;
// internal/gitssh/ssh.go:149-151 classifies a missing exit status as transport failure.
func TestSSHUploadFaultClassification(t *testing.T) {
	g := gitSetup(t)
	for _, tc := range []struct{ kind, needle string }{{"not_found", "repository not found"}, {"forbidden", "permission denied"}, {"server_error", "internal server error"}, {"drop", ""}} {
		t.Run(tc.kind, func(t *testing.T) {
			if err := g.f.Inject(Fault{Method: "git-upload-pack", Path: "org/oberth", Kind: tc.kind, Remaining: 1}); err != nil {
				t.Fatal(err)
			}
			c, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			session, err := c.NewSession()
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			output, err := session.CombinedOutput("git-upload-pack '/org/oberth.git'")
			if tc.kind == "drop" {
				var missing *ssh.ExitMissingError
				if !errors.As(err, &missing) {
					t.Fatalf("classifier needs missing exit status, got %v", err)
				}
			} else if err == nil || !strings.Contains(strings.ToLower(string(output)), tc.needle) {
				t.Fatalf("classifier needle %q absent: %v %s", tc.needle, err, output)
			}
			g.git(t, g.work, "ls-remote", "upstream")
		})
	}
	if state(t, g.f).UnexpectedCount != 0 {
		t.Fatal("upload counted as unexpected")
	}
}
func TestSSHGitProtocols(t *testing.T) {
	g := gitSetup(t)
	for _, version := range []string{"1", "2"} {
		t.Run(version, func(t *testing.T) { assertGitProtocol(t, g, version) })
	}
}
func assertGitProtocol(t *testing.T, g *gitFixture, version string) {
	t.Helper()
	c, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if err := session.Setenv("GIT_PROTOCOL", "version="+version); err != nil {
		t.Fatal(err)
	}
	session.Stdin = strings.NewReader("0000")
	out, err := session.Output("git-upload-pack '/org/oberth.git'")
	if err != nil || !bytes.Contains(out, []byte("version "+version)) {
		t.Fatal("Git protocol advertisement missing")
	}
}
func TestSSHUnexpectedRequestsAndAuthentication(t *testing.T) {
	g := gitSetup(t)
	for _, action := range []string{"LANG", "LC_TEST", "OTHER_ENV", "global", "channel", "subsystem", "shell"} {
		t.Run(action, func(t *testing.T) {
			before := state(t, g.f).UnexpectedCount
			c, err := dial(t, g, g.sign, g.f.signers[0].PublicKey())
			if err != nil {
				t.Fatal(err)
			}
			expected := exerciseSSHRequest(t, c, action)
			c.Close()
			if got := state(t, g.f).UnexpectedCount - before; got != expected {
				t.Fatalf("unexpected delta %d, want %d", got, expected)
			}
		})
	}
}
func TestSSHAuthenticationRefusals(t *testing.T) {
	g := gitSetup(t)
	for _, method := range []string{"password", "keyboard", "wrong-user"} {
		t.Run(method, func(t *testing.T) {
			before := state(t, g.f).UnexpectedCount
			cfg := &ssh.ClientConfig{User: "git", HostKeyCallback: ssh.FixedHostKey(g.f.signers[0].PublicKey()), Timeout: time.Second}
			switch method {
			case "password":
				cfg.Auth = []ssh.AuthMethod{ssh.Password("fixture")}
			case "keyboard":
				cfg.Auth = []ssh.AuthMethod{ssh.KeyboardInteractive(func(string, string, []string, []bool) ([]string, error) { return nil, nil })}
			case "wrong-user":
				cfg.User = "other"
				cfg.Auth = []ssh.AuthMethod{ssh.PublicKeys(g.sign)}
			}
			c, err := ssh.Dial("tcp", g.address, cfg)
			if c != nil {
				c.Close()
			}
			want := 0
			if method == "wrong-user" {
				want = 2
			}
			if err == nil || state(t, g.f).UnexpectedCount-before != want {
				t.Fatalf("authentication accounting delta %d, want %d: %v", state(t, g.f).UnexpectedCount-before, want, err)
			}
		})
	}
}
func TestHostKeyFamiliesAndHermeticGit(t *testing.T) {
	f, err := New(Config{Root: t.TempDir(), Now: clock.RealClock{}.Now})
	if err != nil {
		t.Fatal(err)
	}
	key := signer(t)
	seed(t, f, Seed{Repositories: []RepositorySeed{{Name: "org/oberth"}}, AuthorizedKeys: []string{string(ssh.MarshalAuthorizedKey(key.PublicKey()))}})
	address := startSSH(t, f)
	for _, host := range f.signers {
		algorithm := host.PublicKey().Type()
		if algorithm == ssh.KeyAlgoRSA {
			algorithm = ssh.KeyAlgoRSASHA256
		}
		c, err := ssh.Dial("tcp", address, &ssh.ClientConfig{User: "git", Auth: []ssh.AuthMethod{ssh.PublicKeys(key)}, HostKeyCallback: ssh.FixedHostKey(host.PublicKey()), HostKeyAlgorithms: []string{algorithm}, Timeout: time.Second})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
	}
	if len(state(t, f).HostKeys) != 3 {
		t.Fatal("missing supported host-key family")
	}
	g := gitSetup(t)
	home := t.TempDir()
	badConfig := filepath.Join(home, "gitconfig")
	if err := os.WriteFile(badConfig, []byte("invalid configuration"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("GIT_CONFIG_GLOBAL", badConfig)
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "core.bare")
	t.Setenv("GIT_CONFIG_VALUE_0", "invalid")
	seed(t, g.f, Seed{Repositories: []RepositorySeed{{Name: "org/other"}}})
	g.git(t, g.work, "push", "upstream", "HEAD:refs/heads/main")
	_ = state(t, g.f)
	server, _ := trustServer(t, g.f)
	// An advertisement request isolates CGI configuration from the client config.
	status, _, _ := request(t, server.Client(), testRequest{"GET", server.URL + "/org/oberth.git/info/refs?service=git-upload-pack", nil})
	if status != 200 {
		t.Fatal("CGI read inherited Git config")
	}
}

func TestSSHAdvertisesPublicKeyOnly(t *testing.T) {
	binary, err := exec.LookPath("ssh")
	if err != nil {
		t.Fatal(err)
	}
	g := gitSetup(t)
	var hosts string
	for _, entry := range g.env {
		if value, ok := strings.CutPrefix(entry, "GHFAKE_TEST_HOSTS="); ok {
			hosts = value
		}
	}
	// Supply a key that is not seeded so the server advertises its allowed methods.
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "unregistered-key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(g.address)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-F", "/dev/null", "-v", "-oBatchMode=yes", "-oIdentitiesOnly=yes", "-oStrictHostKeyChecking=yes", "-oUserKnownHostsFile="+hosts, "-i", path, "-p", port, "git@127.0.0.1")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatal("unregistered key authenticated")
	}
	assertPublicKeyOnly(t, string(output))
}
func assertPublicKeyOnly(t *testing.T, output string) {
	t.Helper()
	advertised := false
	for _, line := range strings.Split(output, "\n") {
		if methods, ok := strings.CutPrefix(line, "debug1: Authentications that can continue: "); ok {
			advertised = true
			if strings.TrimSpace(methods) != "publickey" {
				t.Fatalf("authentication methods %q", methods)
			}
		}
	}
	if !advertised {
		t.Fatal("server did not advertise authentication methods")
	}
}

func exerciseSSHRequest(t *testing.T, c *ssh.Client, action string) int {
	t.Helper()
	switch action {
	case "global":
		_, _, _ = c.SendRequest("unexpected@oberth.ci", true, nil)
		return 1
	case "channel":
		_, _, _ = c.OpenChannel("direct-tcpip", nil)
		return 1
	}
	session, err := c.NewSession()
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	switch action {
	case "LANG", "LC_TEST":
		if session.Setenv(action, "C") == nil {
			t.Fatal("locale env should be refused quietly")
		}
		return 0
	case "OTHER_ENV":
		_ = session.Setenv(action, "fixture")
	case "subsystem":
		_ = session.RequestSubsystem("sftp")
	case "shell":
		_ = session.Shell()
	}
	return 1
}
