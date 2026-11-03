package gitssh

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSSHConfigurationAndIdentityRefusalsHaveDistinctClass(t *testing.T) {
	for _, mode := range []string{"configuration", "identity permissions", "invalid pins", "missing pin"} {
		t.Run(mode, func(t *testing.T) {
			key, _ := testKey(t)
			config := configFile(t, "127.0.0.1:2222", key.PublicKey())
			root := filepath.Dir(config)
			var err error
			switch mode {
			case "configuration":
				err = os.WriteFile(config, []byte("ProxyCommand refused\n"), 0600)
			case "identity permissions":
				err = os.Chmod(filepath.Join(root, "identity"), 0644)
			case "invalid pins":
				err = os.WriteFile(filepath.Join(root, "known_hosts"), []byte("invalid known hosts\n"), 0600)
			case "missing pin":
				err = os.WriteFile(filepath.Join(root, "known_hosts"), nil, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			err = Run(t.Context(), []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
			if !errors.Is(err, ErrRefused) || ExitCode(err) != 126 || Diagnostic(err) != RefusalMessage || strings.Contains(err.Error(), root) {
				t.Fatalf("refusal misclassified or leaked configuration: exit=%d diagnostic=%q error=%v", ExitCode(err), Diagnostic(err), err)
			}
		})
	}
}

func TestSSHHandshakeDisconnectIsTemporary(t *testing.T) {
	key, _ := testKey(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	config := configFile(t, listener.Addr().String(), key.PublicKey())
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		connection, err := listener.Accept()
		if err == nil {
			_ = connection.Close()
		}
	}()
	err = Run(t.Context(), []string{"git@127.0.0.1", "git-upload-pack '/fixture'"}, config, io.NopCloser(strings.NewReader("")), io.Discard, io.Discard)
	<-finished
	if !errors.Is(err, ErrTransportUnavailable) || ExitCode(err) != 255 || Diagnostic(err) != TemporaryFailureMessage {
		t.Fatalf("handshake I/O became a policy refusal: exit=%d diagnostic=%q error=%v", ExitCode(err), Diagnostic(err), err)
	}
}
