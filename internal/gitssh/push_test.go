package gitssh

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
	"golang.org/x/crypto/ssh"
)

func TestPushCancellationAndRemoteRefusalKeepTheirBoundaries(t *testing.T) {
	for _, remote := range []bool{false, true} {
		t.Run(fmt.Sprint(remote), func(t *testing.T) {
			host := newSigner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := shimConfig(t, host.PublicKey(), "git-receive-pack 'r.git'")
			cfg.Dial = func(context.Context, string) (net.Conn, error) {
				return loopback(t, servePushResult(host, remote, cancel, 1))
			}
			var diagnostic bytes.Buffer
			err := PushDiagnostic(Run(ctx, cfg, IO{Input: io.NopCloser(strings.NewReader("")), Output: io.Discard, Diagnostics: &diagnostic}))
			if remote {
				var result *ssh.ExitError
				if !errors.As(err, &result) || result.ExitStatus() != 1 || !strings.Contains(diagnostic.String(), "remote refusal") {
					t.Fatal("real remote result was relabelled", err, diagnostic.String())
				}
			} else if publicerr.Classify(err).Reason() != PushUnknownRefusal {
				t.Fatal("post-submission cancellation claimed a safe repeat", err)
			}
		})
	}
}

func TestRawPushCommandRefusalPreservesForgeClassification(t *testing.T) {
	host := newSigner(t)
	cfg := shimConfig(t, host.PublicKey(), "git-receive-pack 'r.git'")
	cfg.Dial = func(context.Context, string) (net.Conn, error) { return loopback(t, serveRefusing(host, false)) }
	err := Run(t.Context(), cfg, IO{Input: io.NopCloser(strings.NewReader("")), Output: io.Discard, Diagnostics: io.Discard})
	if !RefusalFailure(err.Error()) || TransportFailure(err.Error()) {
		t.Fatal("raw forge refusal became transport slowness", err)
	}
	if publicerr.Classify(PushDiagnostic(err)).Reason() != PushUnknownRefusal {
		t.Fatal("client claimed an outcome from Start alone", err)
	}
}

func TestPushCompletedResultSurvivesCancellation(t *testing.T) {
	for _, status := range []uint32{0, 1} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			host := newSigner(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			cfg := shimConfig(t, host.PublicKey(), "git-receive-pack 'r.git'")
			cfg.Dial = func(context.Context, string) (net.Conn, error) {
				return loopback(t, servePushResult(host, true, cancel, status))
			}
			input := &cancelAfterResult{Reader: strings.NewReader(""), cancel: cancel}
			err := PushDiagnostic(Run(ctx, cfg, IO{Input: input, Output: io.Discard, Diagnostics: io.Discard}))
			if ctx.Err() == nil {
				t.Fatal("control did not cancel after Wait")
			}
			if status == 0 {
				if err != nil {
					t.Fatal("completed push became unknown after cancellation", err)
				}
				return
			}
			var result *ssh.ExitError
			if !errors.As(err, &result) || result.ExitStatus() != 1 {
				t.Fatal("completed remote refusal was lost", err)
			}
		})
	}
}

type cancelAfterResult struct {
	io.Reader
	cancel context.CancelFunc
}

func (r *cancelAfterResult) Close() error { r.cancel(); return nil }

func servePushResult(host ssh.Signer, remote bool, cancel context.CancelFunc, status uint32) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
		cfg.AddHostKey(host)
		conn, channels, requests, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		incoming, ok := <-channels
		if !ok {
			return
		}
		channel, reqs, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		req := <-reqs
		_ = req.Reply(true, nil)
		if !remote {
			cancel()
			_ = conn.Wait()
			return
		}
		if status != 0 {
			_, _ = channel.Stderr().Write([]byte("remote refusal\n"))
		}
		_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
	}
}

func TestPushBoundaryRequiresEvidenceBeforeAdvisingAnotherPush(t *testing.T) {
	host := newSigner(t)
	cfg := shimConfig(t, host.PublicKey(), "git-receive-pack 'r.git'")
	_, configErr := Configure(filepath.Join(cfg.Scratch, "absent-config"), cfg)
	if publicerr.Classify(PushDiagnostic(configErr)).Reason() != PushLocalRefusal {
		t.Fatal("configuration failure lost its preconnection boundary", configErr)
	}
	for _, tc := range []struct {
		name, want string
		serve      func(net.Conn)
	}{
		{"dial", PushLocalRefusal, nil},
		{"handshake", PushLocalRefusal, func(c net.Conn) { _ = c.Close() }},
		{"ambiguous start", PushUnknownRefusal, serveAmbiguousStart(host)},
		{"after send", PushUnknownRefusal, serveThenDrop(host, true)},
		{"identity refused", ReasonIdentityRefused, serveThenDrop(host, false)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := shimConfig(t, host.PublicKey(), "git-receive-pack 'r.git'")
			cfg.Dial = func(context.Context, string) (net.Conn, error) {
				if tc.serve == nil {
					return nil, errors.New("dial failed")
				}
				return loopback(t, tc.serve)
			}
			err := PushDiagnostic(Run(t.Context(), cfg, IO{Input: io.NopCloser(strings.NewReader("")), Output: io.Discard, Diagnostics: io.Discard}))
			if got := publicerr.Classify(err).Reason(); got != tc.want {
				t.Fatalf("%s: %v", got, err)
			}
		})
	}
	// An external SSH command's rc128 has no trusted boundary evidence.
	unknown := errors.New("exit status 128")
	if !errors.Is(PushDiagnostic(unknown), unknown) || publicerr.Classify(PushDiagnostic(unknown)).Code() != publicerr.Invariant {
		t.Fatal("inferred transport stage from exit status")
	}
	for text, bound := range map[string]int{PushLocalReason: 40, PushLocalRefusal: 80, PushLocalRemedy: 120, PushUnknownReason: 40, PushUnknownRefusal: 80, PushUnknownRemedy: 120} {
		if len(text) > bound {
			t.Fatal("message exceeds bound", text)
		}
	}
}

func serveAmbiguousStart(host ssh.Signer) func(net.Conn) {
	return func(c net.Conn) {
		defer c.Close()
		cfg := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
		cfg.AddHostKey(host)
		conn, channels, requests, err := ssh.NewServerConn(c, cfg)
		if err != nil {
			return
		}
		defer conn.Close()
		go ssh.DiscardRequests(requests)
		incoming, ok := <-channels
		if !ok {
			return
		}
		channel, reqs, err := incoming.Accept()
		if err != nil {
			return
		}
		defer channel.Close()
		<-reqs // Receive exec, then lose the acknowledgement.
	}
}
