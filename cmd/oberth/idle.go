package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

// runWaitIdle is an in-pod read of the live daemon, used by upgrade --wait-idle.
// The private socket avoids opening a second owner or probing a scoped API view.
func runWaitIdle(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("wait-idle", flag.ContinueOnError)
	flags.SetOutput(output)
	timeout := flags.Duration("timeout", 5*time.Minute, "maximum time to wait for the live daemon queue (in-pod)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 {
		return fmt.Errorf("%w: wait-idle accepts a positive --timeout only", errUsage)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", defaultAdminSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	if err := waitIdle(ctx, client, "http://admin/v1/idle", time.Second); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, "queue is idle")
	return err
}

func waitIdle(ctx context.Context, client *http.Client, target string, interval time.Duration) error {
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
		if err != nil {
			return err
		}
		response, err := client.Do(request)
		if err != nil {
			return fmt.Errorf("read live daemon queue: %w", err)
		}
		_ = response.Body.Close()
		switch response.StatusCode {
		case http.StatusNoContent:
			return nil
		case http.StatusConflict: // A live, busy queue; keep observing within the deadline.
		default:
			return fmt.Errorf("live daemon queue unavailable (HTTP %d)", response.StatusCode)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}
