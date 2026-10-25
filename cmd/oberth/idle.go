package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
)

// runWaitIdle is an in-pod read of the live daemon, used by upgrade --wait-idle.
// The private socket avoids opening a second owner or probing a scoped API view.
func runWaitIdle(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("wait-idle", flag.ContinueOnError)
	flags.SetOutput(output)
	timeout := flags.Duration("timeout", installer.DefaultUpgradeWaitTimeout, "maximum time to wait for the live daemon queue (in-pod)")
	mode := flags.String("wait-for", "all", "runs to wait for: all or critical (tag/promotion/plan/apply/unknown)")
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	if flags.NArg() != 0 || *timeout <= 0 || (*mode != "all" && *mode != "critical") {
		return fmt.Errorf("%w: wait-idle requires a positive --timeout and --wait-for=all|critical", errUsage)
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", defaultAdminSocket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
	if err := waitIdle(ctx, client, "http://admin/v1/idle?wait_for="+*mode, time.Second, idleWaitOptions{output: output, mode: *mode}); err != nil {
		return fmt.Errorf("wait for %s queue (limit=%s): %w", *mode, *timeout, err)
	}
	_, err := fmt.Fprintf(output, "%s queue is idle\n", *mode)
	return err
}

type idleWaitOptions struct {
	output io.Writer
	mode   string
}

func waitIdle(ctx context.Context, client *http.Client, target string, interval time.Duration, options ...idleWaitOptions) error {
	opt := idleWaitOptions{output: io.Discard, mode: "all"}
	if len(options) > 0 {
		opt = options[0]
	}
	if opt.output == nil {
		opt.output = io.Discard
	}
	start := time.Now()
	nextProgress := start
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil) //nolint:gosec // G704: production uses a fixed admin Unix-socket URL; dynamic loopback targets are test fixtures.
		if err != nil {
			return err
		}
		response, err := client.Do(request) //nolint:gosec // G704: the production transport always dials the fixed private Unix socket.
		if err != nil {
			return fmt.Errorf("read live daemon queue: %w", err)
		}
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		closeErr := response.Body.Close()
		if readErr != nil || closeErr != nil {
			return fmt.Errorf("read live daemon queue: %w", errors.Join(readErr, closeErr))
		}
		switch response.StatusCode {
		case http.StatusNoContent, http.StatusConflict:
			if opt.mode == "critical" && response.Header.Get("X-Oberth-Idle-Version") != "1" {
				return errors.New("server does not support critical queue observation; upgrade its wait-idle support first or use --wait-for=all")
			}
			if response.StatusCode == http.StatusNoContent {
				return ctx.Err()
			}
		default:
			return fmt.Errorf("live daemon queue unavailable (HTTP %d)", response.StatusCode)
		}
		var state model.IdleQueueStatus
		known := len(body) > 0
		if known && (len(body) > 4096 || json.Unmarshal(body, &state) != nil || !state.Valid()) {
			return errors.New("invalid live daemon queue snapshot")
		}
		if !time.Now().Before(nextProgress) {
			elapsed := time.Since(start).Truncate(time.Second)
			var err error
			if known {
				_, err = fmt.Fprintf(opt.output, "Waiting for %s queue: queued=%d running=%d branch=%d critical=%d (tag=%d promotion=%d plan=%d apply=%d other=%d) elapsed=%s\n", opt.mode, state.Queued, state.Running, state.Branch, state.Critical(), state.Tag, state.Promotion, state.Plan, state.Apply, state.Other, elapsed)
			} else {
				_, err = fmt.Fprintf(opt.output, "Waiting for %s queue: busy (counts unavailable on this server) elapsed=%s\n", opt.mode, elapsed)
			}
			if err != nil {
				return fmt.Errorf("write idle progress: %w", err)
			}
			nextProgress = time.Now().Add(15 * time.Second)
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
