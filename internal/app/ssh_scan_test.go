package app

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/oberthci/oberth/internal/testtimeout"
)

func TestScanSSHHostKeysDiscoversFamiliesWithoutAuthentication(t *testing.T) {
	t.Parallel()
	_, ed, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	var authCalls atomic.Int64
	config := &ssh.ServerConfig{NoClientAuth: true, NoClientAuthCallback: func(ssh.ConnMetadata) (*ssh.Permissions, error) { authCalls.Add(1); return nil, nil }}
	expected := map[string]bool{}
	for _, key := range []any{ed, ec, rsaKey} {
		signer, err := ssh.NewSignerFromKey(key)
		if err != nil {
			t.Fatal(err)
		}
		config.AddHostKey(signer)
		expected[ssh.FingerprintSHA256(signer.PublicKey())] = true
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	workers.Go(func() {
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			workers.Go(func() {
				defer func() { _ = connection.Close() }()
				stop := context.AfterFunc(ctx, func() { _ = connection.Close() })
				defer stop()
				server, channels, requests, err := ssh.NewServerConn(connection, config)
				if err != nil {
					return
				}
				defer func() { _ = server.Close() }()
				go ssh.DiscardRequests(requests)
				for channel := range channels {
					_ = channel.Reject(ssh.Prohibited, "probe must not open channels")
				}
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); workers.Wait() })
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	keys, err := ScanSSHHostKeys(t.Context(), host, port)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 3 {
		t.Fatalf("discovered %d keys, want all three families", len(keys))
	}
	for _, key := range keys {
		fingerprint := ssh.FingerprintSHA256(key)
		if !expected[fingerprint] {
			t.Fatalf("unexpected key %s", fingerprint)
		}
		delete(expected, fingerprint)
	}
	if len(expected) != 0 || authCalls.Load() != 0 {
		t.Fatalf("remaining keys=%v authentication attempts=%d", expected, authCalls.Load())
	}
}

func TestScanSSHHostKeysCancellationClosesStalledHandshake(t *testing.T) {
	t.Parallel()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = io.Copy(io.Discard, connection)
	}()
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err = ScanSSHHostKeys(ctx, host, port)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe error=%v", err)
	}
	select {
	case <-joined:
	case <-time.After(testtimeout.Start(time.Second)):
		t.Fatal("cancelled handshake retained its TCP connection")
	}
}

func TestScanSSHHostKeysStalledFamilyDoesNotStarveOthers(t *testing.T) {
	t.Parallel()
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(private)
	if err != nil {
		t.Fatal(err)
	}
	config := &ssh.ServerConfig{NoClientAuth: true}
	config.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	var workers sync.WaitGroup
	var accepts atomic.Int64
	workers.Go(func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			number := accepts.Add(1)
			workers.Go(func() {
				defer func() { _ = conn.Close() }()
				stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
				defer stop()
				// ECDSA stalls after ED25519 has already succeeded. The RSA probe must
				// still run rather than inheriting the exhausted aggregate deadline.
				if number == 2 {
					_, _ = io.Copy(io.Discard, conn)
					return
				}
				client, _, _, err := ssh.NewServerConn(conn, config)
				if err == nil {
					_ = client.Close()
				}
			})
		}
	})
	t.Cleanup(func() { cancel(); _ = listener.Close(); workers.Wait() })
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	keys, err := scanSSHHostKeys(t.Context(), host, port, 100*time.Millisecond)
	if err != nil || len(keys) != 1 {
		t.Fatalf("probe lost successful family: keys=%d err=%v", len(keys), err)
	}
	if accepts.Load() != 3 {
		t.Fatalf("later family starved: connections=%d", accepts.Load())
	}
}
