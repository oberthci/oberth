package gitcache

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/go-git/go-git/v5/plumbing/protocol/packp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestGoGitSSHPushAndCertificateAuthority(t *testing.T) {
	for _, certified := range []bool{false, true} {
		t.Run(map[bool]string{false: "pinned key", true: "pinned CA"}[certified], func(t *testing.T) {
			repository := newTestRepository(t)
			candidate := repository.featureCommit(t, "feature/ssh-push", "SSH push\n")
			identity, keyBody := testSSHSigner(t)
			host, _ := testSSHSigner(t)
			var authority ssh.Signer
			pin := host.PublicKey()
			if certified {
				rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
				if err != nil {
					t.Fatal(err)
				}
				authority, err = ssh.NewSignerFromKey(rsaKey)
				if err != nil {
					t.Fatal(err)
				}
				cert := &ssh.Certificate{Key: host.PublicKey(), CertType: ssh.HostCert, KeyId: "fixture", ValidPrincipals: []string{"127.0.0.1"}, ValidAfter: uint64(time.Now().Add(-time.Minute).Unix()), ValidBefore: uint64(time.Now().Add(time.Hour).Unix())}
				if err := cert.SignCert(rand.Reader, authority); err != nil {
					t.Fatal(err)
				}
				host, err = ssh.NewCertSigner(cert, host)
				if err != nil {
					t.Fatal(err)
				}
				pin = authority.PublicKey()
			}
			fixture := startGitSSHFixtureWithHost(t, repository.upstream, identity.PublicKey(), host)
			address := strings.TrimPrefix(strings.TrimSuffix(fixture.url, repository.upstream), "ssh://git@")
			root := t.TempDir()
			key := filepath.Join(root, "identity")
			hosts := filepath.Join(root, "known_hosts")
			if err := os.WriteFile(key, keyBody, 0o600); err != nil {
				t.Fatal(err)
			}
			marker := ""
			if certified {
				marker = "@cert-authority "
			}
			line := marker + knownhosts.Line([]string{knownhosts.Normalize(address)}, pin) + "\n"
			if err := os.WriteFile(hosts, []byte(line), 0o600); err != nil {
				t.Fatal(err)
			}
			cache, err := New(Config{Root: filepath.Join(root, "cache"), Backend: BackendGoGit, SSH: SSHConfig{PrivateKeyPath: key, KnownHostsPath: hosts}, Upstream: func(string) (string, error) { return fixture.url, nil }})
			if err != nil {
				t.Fatal(err)
			}
			ready, err := cache.Ensure(t.Context(), "example")
			if err != nil {
				t.Fatal(err)
			}
			seedCacheObject(t, repository.work, ready.Path, candidate)
			t.Setenv("PATH", t.TempDir())
			if err := cache.run(t.Context(), commandSpec{dir: ready.Path, args: []string{"push", fixture.url, candidate + ":refs/heads/feature/ssh-push"}}); err != nil {
				t.Fatal(err)
			}
			// Read back through SSH, independently of the push report-status.
			got, exists, err := cache.RemoteRef(t.Context(), "example", "refs/heads/feature/ssh-push")
			if err != nil || !exists || got != candidate {
				t.Fatalf("SSH push readback=%s %v %v", got, exists, err)
			}
			if certified {
				other, _ := testSSHSigner(t)
				if err := os.WriteFile(hosts, []byte("@cert-authority "+knownhosts.Line([]string{address}, other.PublicKey())+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				calls := fixture.authCalls.Load()
				if _, err := cache.LsRemoteHeads(t.Context(), "example"); err == nil {
					t.Fatal("untrusted CA accepted")
				}
				if fixture.authCalls.Load() != calls {
					t.Fatal("identity offered to untrusted CA")
				}
			}
		})
	}
}

func TestPinnedRSAAlgorithmsExcludeSHA1(t *testing.T) {
	identity, body := testSSHSigner(t)
	_ = identity
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(rsaKey)
	if err != nil {
		t.Fatal(err)
	}
	for _, marker := range []string{"", "@cert-authority "} {
		t.Run(marker, func(t *testing.T) {
			root := t.TempDir()
			key := filepath.Join(root, "key")
			hosts := filepath.Join(root, "hosts")
			if err := os.WriteFile(key, body, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(hosts, []byte(marker+knownhosts.Line([]string{"rsa.example"}, signer.PublicKey())+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cache, err := New(Config{Root: filepath.Join(root, "cache"), Backend: BackendGoGit, SSH: SSHConfig{PrivateKeyPath: key, KnownHostsPath: hosts}, Upstream: func(string) (string, error) { return "unused", nil }})
			if err != nil {
				t.Fatal(err)
			}
			_, auth, err := cache.backend.(*goGitBackend).remoteAuth(t.Context(), "ssh://git@rsa.example/repo.git")
			if err != nil {
				t.Fatal(err)
			}
			algorithms := auth.(*strictSSHAuth).config.HostKeyAlgorithms
			if slices.Contains(algorithms, ssh.KeyAlgoRSA) || slices.Contains(algorithms, ssh.CertAlgoRSAv01) {
				t.Fatalf("SHA-1 RSA enabled: %v", algorithms)
			}
			modern := ssh.KeyAlgoRSASHA256
			if marker != "" {
				modern = ssh.CertAlgoRSASHA256v01
			}
			if !slices.Contains(algorithms, modern) {
				t.Fatalf("modern RSA removed: %v", algorithms)
			}
		})
	}
}

type recordingSSHInput struct {
	bytes.Buffer
	closed bool
}

func (w *recordingSSHInput) Close() error { w.closed = true; return nil }
func TestSSHRefusesUnattributablePushBeforeWriting(t *testing.T) {
	local, remote := net.Pipe()
	defer func() { _ = remote.Close() }()
	input := &recordingSSHInput{}
	session := &strictSSHSession{ctx: context.Background(), connection: local, stop: func() bool { return true }, stdin: input, stdout: strings.NewReader("")}
	request := packp.NewReferenceUpdateRequest()
	request.Packfile = io.NopCloser(strings.NewReader("must not be sent"))
	if _, err := session.ReceivePack(t.Context(), request); err == nil || !strings.Contains(err.Error(), "report-status") {
		t.Fatalf("unattributable push=%v", err)
	}
	if input.Len() != 0 || input.closed {
		t.Fatal("push wrote/closed stdin before checking report-status")
	}
}
