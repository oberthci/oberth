package api_test

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/apicontract"
)

func TestSlowPeersReachListenerTimeouts(t *testing.T) {
	for _, tc := range []struct {
		name string
		wait time.Duration
	}{
		{"headers", 10 * time.Second}, {"body", time.Minute}, {"idle", 2 * time.Minute}, {"write", 16 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				done := make(chan error, 1)
				f := setup(t, func(c *api.Config, _ *apicontract.Bindings) {
					c.Runtime.Now = testNow
					c.Source = slowPeerHandler(tc.name, done)
				})
				peer := slowPeer(t, f.server.SourceHandler())
				start := testNow()
				driveSlowPeer(t, peer, tc.name, done)
				if elapsed := testNow().Sub(start); elapsed != tc.wait {
					t.Fatalf("timeout after %s, want %s", elapsed, tc.wait)
				}
			})
		})
	}
}

func slowPeerHandler(name string, done chan<- error) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch name {
		case "body":
			_, err := io.ReadAll(r.Body)
			done <- err
		case "write":
			_, err := io.WriteString(w, strings.Repeat("x", 128<<10))
			done <- err
		default:
			w.WriteHeader(204)
		}
	})
}
func driveSlowPeer(t *testing.T, peer net.Conn, name string, done <-chan error) {
	t.Helper()
	request := "GET / HTTP/1.1\r\nHost: memory\r\n\r\n"
	if name == "headers" {
		request = "GET / HTTP/1.1\r\nHost: memory\r\nX: "
	}
	if name == "body" {
		request = "POST / HTTP/1.1\r\nHost: memory\r\nContent-Length: 4\r\n\r\na"
	}
	if _, err := io.WriteString(peer, request); err != nil {
		t.Fatal(err)
	}
	switch name {
	case "body", "write":
		if err := <-done; err == nil {
			t.Fatal("slow I/O succeeded")
		}
	case "idle":
		r, err := http.ReadResponse(bufio.NewReader(peer), nil)
		if err != nil {
			t.Fatal(err)
		}
		_ = r.Body.Close()
		if _, err = peer.Read(make([]byte, 1)); err == nil {
			t.Fatal("idle peer survived")
		}
	default:
		_, _ = io.ReadAll(peer)
	}
}
func slowPeer(t *testing.T, handler http.Handler) net.Conn {
	t.Helper()
	l := &secondListener{pending: make(chan net.Conn), done: make(chan struct{})}
	server := api.ListenerServer(handler)
	stopped := make(chan struct{})
	go func() { defer close(stopped); _ = server.Serve(l) }()
	peer, err := l.dial(context.Background(), "", "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close(); _ = server.Close(); <-stopped })
	return peer
}
