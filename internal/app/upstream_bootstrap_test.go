package app

import (
	"context"
	"crypto/ed25519"
	cryptorand "crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

func TestProbeSSHCapabilityAuthenticatesAndRequestsWriteCommandWithoutUpdates(t *testing.T) {
	t.Parallel()
	clientPublic, clientPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	clientSSHKey, err := ssh.NewPublicKey(clientPublic)
	if err != nil {
		t.Fatal(err)
	}
	hostPublic, hostPrivate, err := ed25519.GenerateKey(cryptorand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	hostSigner, err := ssh.NewSignerFromKey(hostPrivate)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })

	serverErrors := make(chan error, 1)
	commands := make(chan string, 1)
	go serveCapabilityProbe(listener, hostSigner, clientSSHKey, commands, serverErrors)

	privateBlock, err := ssh.MarshalPrivateKey(clientPrivate, "test")
	if err != nil {
		t.Fatal(err)
	}
	hostKey, err := ssh.NewPublicKey(hostPublic)
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	knownHosts := []byte(knownhosts.Line([]string{address}, hostKey) + "\n")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := ProbeSSHCapability(ctx, "ssh://git@"+address+"/cloudtaser", pem.EncodeToMemory(privateBlock), knownHosts); err != nil {
		t.Fatal(err)
	}
	select {
	case command := <-commands:
		if command != "git-receive-pack '/cloudtaser/oberth.git'" {
			t.Fatalf("probe command = %q", command)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func serveCapabilityProbe(listener net.Listener, hostSigner ssh.Signer, allowedKey ssh.PublicKey, commands chan<- string, result chan<- error) {
	connection, err := listener.Accept()
	if err != nil {
		result <- err
		return
	}
	defer connection.Close()
	configuration := &ssh.ServerConfig{
		PublicKeyCallback: func(metadata ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if metadata.User() != "git" || !strings.EqualFold(ssh.FingerprintSHA256(key), ssh.FingerprintSHA256(allowedKey)) {
				return nil, fmt.Errorf("unexpected probe identity %q", metadata.User())
			}
			return nil, nil
		},
	}
	configuration.AddHostKey(hostSigner)
	server, channels, requests, err := ssh.NewServerConn(connection, configuration)
	if err != nil {
		result <- err
		return
	}
	defer server.Close()
	go ssh.DiscardRequests(requests)
	for incoming := range channels {
		if incoming.ChannelType() != "session" {
			_ = incoming.Reject(ssh.UnknownChannelType, "session required")
			continue
		}
		channel, channelRequests, err := incoming.Accept()
		if err != nil {
			result <- err
			return
		}
		for request := range channelRequests {
			if request.Type != "exec" {
				_ = request.Reply(false, nil)
				continue
			}
			var payload struct{ Command string }
			if err := ssh.Unmarshal(request.Payload, &payload); err != nil {
				result <- err
				return
			}
			commands <- payload.Command
			if err := request.Reply(true, nil); err != nil {
				result <- err
				return
			}
			_, _ = channel.Write([]byte("0000"))
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{Status: 0}))
			_ = channel.Close()
			result <- nil
			return
		}
	}
	result <- errors.New("probe session closed before an exec request")
}

func TestGitAdvertisementRejectsWriteDenial(t *testing.T) {
	payload := "ERR write access denied\n"
	denied := fmt.Sprintf("%04x%s", len(payload)+4, payload)
	if err := readGitAdvertisement(strings.NewReader(denied)); err == nil {
		t.Fatal("accepted Git write denial")
	}
	if err := readGitAdvertisement(strings.NewReader("0000")); err != nil {
		t.Fatalf("rejected empty Git advertisement: %v", err)
	}
}
