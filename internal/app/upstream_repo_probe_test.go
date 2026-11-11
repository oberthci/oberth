package app

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"

	"github.com/oberthci/oberth/internal/testtimeout"
)

// startGitShellForge serves git-upload-pack like a plain git-shell account:
// repositories live relative to the git user's home, so only "~/..." paths
// exist and an absolute path is refused with git's own message.
func startGitShellForge(t *testing.T, client ssh.PublicKey, repositories map[string]bool) (string, []byte) {
	t.Helper()
	_, host, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(host)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{PublicKeyCallback: func(_ ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
		if string(key.Marshal()) != string(client.Marshal()) {
			return nil, errors.New("unknown key")
		}
		return nil, nil
	}}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer func() { _ = connection.Close() }()
				_ = connection.SetDeadline(time.Now().Add(testtimeout.Start(10 * time.Second)))
				server, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, channelRequests, err := incoming.Accept()
					if err != nil {
						return
					}
					serveUploadPack(channel, channelRequests, repositories)
				}
			})
		}
	})
	t.Cleanup(func() { _ = listener.Close(); workers.Wait() })
	address := listener.Addr().String()
	return address, []byte(knownhosts.Line([]string{address}, signer.PublicKey()) + "\n")
}

func serveUploadPack(channel ssh.Channel, requests <-chan *ssh.Request, repositories map[string]bool) {
	defer func() { _ = channel.Close() }()
	for request := range requests {
		if request.Type != "exec" {
			_ = request.Reply(false, nil)
			continue
		}
		_ = request.Reply(true, nil)
		length := binary.BigEndian.Uint32(request.Payload[:4])
		command := string(request.Payload[4 : 4+length])
		path := strings.TrimSuffix(strings.TrimPrefix(command, "git-upload-pack '"), "'")
		status := uint32(0)
		if repositories[path] {
			_, _ = io.WriteString(channel, "003e0000000000000000000000000000000000000000 capabilities^{}\x00\n0000")
			buffer := make([]byte, 4)
			_, _ = io.ReadFull(channel, buffer)
		} else {
			_, _ = io.WriteString(channel.Stderr(), "fatal: '"+path+"' does not appear to be a git repository\n")
			status = 128
		}
		_, _ = channel.SendRequest("exit-status", false, binary.BigEndian.AppendUint32(nil, status))
		return
	}
}

func TestProbeSSHRepositoryNamesPathAndSuggestsHomeRelativeForm(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	address, pins := startGitShellForge(t, client, map[string]bool{"~/acme/demo.git": true})
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(20*time.Second))
	defer cancel()

	if err := ProbeSSHRepository(ctx, "ssh://git@"+address+"/~/acme", "demo", private, pins); err != nil {
		t.Fatalf("home-relative path: %v", err)
	}
	err := ProbeSSHRepository(ctx, "ssh://git@"+address+"/acme", "demo", private, pins)
	if !errors.Is(err, ErrUpstreamRepositoryNotFound) {
		t.Fatalf("absolute path error = %v, want ErrUpstreamRepositoryNotFound", err)
	}
	for _, want := range []string{"/acme/demo.git", "does not appear to be a git repository", "ssh://git@" + address + "/~/acme"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if err := ProbeSSHRepository(ctx, "ssh://git@"+address+"/~/acme", "missing", private, pins); !errors.Is(err, ErrUpstreamRepositoryNotFound) || strings.Contains(err.Error(), "relative to the git user's home") {
		t.Fatalf("missing home-relative repository error = %v", err)
	}
}

func TestUpstreamBootstrapCheckRepositoryRefusesWrongOrganizationPath(t *testing.T) {
	t.Parallel()
	client, private := testClientIdentity(t)
	address, pins := startGitShellForge(t, client, map[string]bool{"~/acme/demo.git": true})
	store := &memoryIdentityStore{data: map[string]map[string][]byte{
		"oberth-upstream-key": {"id_ed25519": private, "id_ed25519.pub": ssh.MarshalAuthorizedKey(client)},
		"oberth-known-hosts":  {"known_hosts": pins},
	}}
	ctx, cancel := context.WithTimeout(context.Background(), testtimeout.Start(20*time.Second))
	defer cancel()
	bootstrap := repinBootstrap(t, store, "")
	bootstrap.CheckRepository = "demo"
	if _, err := bootstrap.Ensure(ctx, "ssh://git@"+address+"/acme"); !errors.Is(err, ErrUpstreamRepositoryNotFound) {
		t.Fatalf("absolute organization path = %v, want a repository check failure", err)
	}
	if _, err := bootstrap.Ensure(ctx, "ssh://git@"+address+"/~/acme"); err != nil {
		t.Fatalf("home-relative organization path: %v", err)
	}
}

func TestUpstreamRepositoryPath(t *testing.T) {
	t.Parallel()
	for base, want := range map[string]string{
		"ssh://git@forge.example/acme":        "/acme/demo.git",
		"ssh://git@forge.example:2222/~/acme": "~/acme/demo.git",
		"ssh://git@forge.example/~git/acme":   "~git/acme/demo.git",
	} {
		got, err := UpstreamRepositoryPath(base, "demo")
		if err != nil || got != want {
			t.Errorf("UpstreamRepositoryPath(%q) = %q, %v; want %q", base, got, err, want)
		}
	}
}
