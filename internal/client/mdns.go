package client

import (
	"context"
	"errors"
	"net"
	"os/exec"
	"strings"
	"time"
)

// DialContext uses the host resolver for .local names while TLS keeps the requested name.
func DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return dialWithFallback(ctx, network, address, dialFunctions{dialer.DialContext, resolveLocal})
}

func dialWithFallback(ctx context.Context, network, address string, functions dialFunctions) (net.Conn, error) {
	conn, err := functions.dial(ctx, network, address)
	var dns *net.DNSError
	host, port, splitErr := net.SplitHostPort(address)
	if err == nil || splitErr != nil || !strings.HasSuffix(strings.TrimSuffix(strings.ToLower(host), "."), ".local") || !errors.As(err, &dns) {
		return conn, err
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return nil, ctx.Err()
	}
	connection, fallbackErr := functions.fallback(ctx, network, net.JoinHostPort(host, port))
	if fallbackErr != nil {
		if errors.Is(ctx.Err(), context.Canceled) {
			return nil, ctx.Err()
		}
		var connectionError *net.OpError
		if errors.As(fallbackErr, &connectionError) {
			return nil, fallbackErr
		}
		return nil, failure("local_dns", errors.Join(err, fallbackErr))
	}
	return connection, nil
}

func resolveLocal(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := exec.CommandContext(ctx, "getent", "ahosts", "--", host).Output()
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var last error
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || net.ParseIP(fields[0]) == nil || seen[fields[0]] {
			continue
		}
		seen[fields[0]] = true
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(fields[0], port))
		if dialErr == nil {
			return conn, nil
		}
		last = dialErr
	}
	if last != nil {
		return nil, last
	}
	return nil, &net.DNSError{Err: "system resolver failed", Name: host}
}

type dialFunctions struct {
	dial, fallback func(context.Context, string, string) (net.Conn, error)
}
