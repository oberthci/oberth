package client

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestLocalConnectionFailureKeepsItsNetworkCause(t *testing.T) {
	want := &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED}
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, &net.DNSError{Err: "not found"} }
	fallback := func(context.Context, string, string) (net.Conn, error) { return nil, want }
	_, err := dialWithFallback(t.Context(), "tcp", "oberth.local:443", dialFunctions{dial, fallback})
	var problem *publicerr.Error
	var network *net.OpError
	if errors.As(err, &problem) || !errors.As(err, &network) || network != want {
		t.Fatal("resolved .local connection failure became a name refusal", err)
	}
}

func TestHostResolverPreservesConnectionFailure(t *testing.T) {
	dir := t.TempDir()
	script := []byte("#!/bin/sh\nprintf '127.0.0.1 STREAM endpoint\\n'\n")
	if err := os.WriteFile(filepath.Join(dir, "getent"), script, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	_, err := resolveLocal(t.Context(), "fixture-network", "oberth.local:443")
	var network net.UnknownNetworkError
	if !errors.As(err, &network) {
		t.Fatal("host resolver discarded a resolved connection failure", err)
	}
}

func TestUnresolvableLocalNameHasActionableRefusal(t *testing.T) {
	for _, address := range []string{"oberth.local:443", "OBERTH.LOCAL.:443"} {
		original := &net.OpError{Op: "dial", Err: &net.DNSError{Name: "oberth.local", Err: "not found"}}
		dial := func(context.Context, string, string) (net.Conn, error) { return nil, original }
		fallback := func(_ context.Context, _, got string) (net.Conn, error) {
			if got != address {
				t.Fatal("resolver changed the requested endpoint", got)
			}
			return nil, errors.New("host resolver unavailable")
		}
		_, err := dialWithFallback(t.Context(), "tcp", address, dialFunctions{dial, fallback})
		var problem *publicerr.Error
		if !errors.As(err, &problem) || problem.Reason() != "Cannot resolve this .local server name." || problem.Remedy() != "Use an IP address or a DNS name this client can resolve, then rerun the command." || retryRequest(err) {
			t.Fatal("unresolved local name lacks a terminal actionable refusal", err)
		}
	}
}

func TestLocalFallbackKeepsSuccessfulConnectionAndCancellation(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	dial := func(context.Context, string, string) (net.Conn, error) { return nil, &net.DNSError{Err: "not found"} }
	fallback := func(context.Context, string, string) (net.Conn, error) { return left, nil }
	conn, err := dialWithFallback(t.Context(), "tcp", "oberth.local:443", dialFunctions{dial, fallback})
	if err != nil || conn != left {
		t.Fatal("host resolution lost a usable connection", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	conn, err = dialWithFallback(ctx, "tcp", "oberth.local:443", dialFunctions{dial, fallback})
	if !errors.Is(err, context.Canceled) || conn != nil {
		t.Fatal("cancelled resolution connected", err)
	}
	ctx, stop := context.WithCancel(t.Context())
	defer stop()
	fallback = func(context.Context, string, string) (net.Conn, error) {
		stop()
		return nil, context.Canceled
	}
	_, err = dialWithFallback(ctx, "tcp", "oberth.local:443", dialFunctions{dial, fallback})
	var problem *publicerr.Error
	if !errors.Is(err, context.Canceled) || errors.As(err, &problem) {
		t.Fatal("cancelled host resolver became an address refusal", err)
	}
}
