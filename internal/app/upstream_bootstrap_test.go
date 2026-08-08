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

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
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

func TestApplySecretForcesOwnershipOfHelmClaimedFields(t *testing.T) {
	t.Parallel()
	clientset := fake.NewClientset()
	bootstrap := UpstreamSSHBootstrap{
		Namespace: "oberth",
		MutationGate: func(context.Context, string) error {
			return nil
		},
	}
	if err := bootstrap.applySecret(context.Background(), clientset, "oberth-known-hosts", map[string][]byte{
		"known_hosts": []byte("codeberg.org ssh-ed25519 AAAA"),
	}); err != nil {
		t.Fatalf("applySecret: %v", err)
	}
	var patches []clienttesting.PatchActionImpl
	for _, action := range clientset.Actions() {
		if patch, ok := action.(clienttesting.PatchActionImpl); ok {
			patches = append(patches, patch)
		}
	}
	if len(patches) != 1 {
		t.Fatalf("expected exactly one patch action, got %d", len(patches))
	}
	patch := patches[0]
	if patch.PatchType != types.ApplyPatchType {
		t.Fatalf("expected server-side apply, got %q", patch.PatchType)
	}
	if patch.PatchOptions.FieldManager != bootstrapFieldManager {
		t.Fatalf("expected field manager %q, got %q", bootstrapFieldManager, patch.PatchOptions.FieldManager)
	}
	// The chart's zero-prerequisite install pre-creates the upstream Secrets
	// with helm owning .data; a non-forced apply fails closed against that
	// placeholder ownership and breaks the first `oberth upstream add`.
	if patch.PatchOptions.Force == nil || !*patch.PatchOptions.Force {
		t.Fatal("bootstrap server-side apply must force ownership of its fields")
	}
}

func TestApplySecretRefusesWithoutMutationGate(t *testing.T) {
	t.Parallel()
	clientset := fake.NewSimpleClientset()
	bootstrap := UpstreamSSHBootstrap{Namespace: "oberth"}
	err := bootstrap.applySecret(context.Background(), clientset, "oberth-known-hosts", nil)
	if err == nil {
		t.Fatal("expected applySecret to fail without a mutation gate")
	}
	if len(clientset.Actions()) != 0 {
		t.Fatalf("expected no API actions without a mutation gate, got %d", len(clientset.Actions()))
	}
}
