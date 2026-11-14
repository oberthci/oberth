package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/recordjson"
)

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
	nextProgress, nextFailure := start, start
	backoff := interval
	var lastFailure error
	finish := func(err error) error {
		if lastFailure != nil {
			return fmt.Errorf("%w; last idle probe failure: %w", err, lastFailure)
		}
		return err
	}
	for {
		if err := ctx.Err(); err != nil {
			return finish(err)
		}
		state, busy, known, transient, err := probeIdle(ctx, client, target, opt.mode)
		delay := interval
		if err != nil {
			if !transient {
				return err
			}
			lastFailure = err
			if ctx.Err() != nil {
				return finish(ctx.Err())
			}
			if !time.Now().Before(nextFailure) {
				if _, writeErr := fmt.Fprintf(opt.output, "Waiting for %s queue: transient idle probe failure: %v; retrying (elapsed=%s)\n", opt.mode, err, time.Since(start).Truncate(time.Second)); writeErr != nil {
					return fmt.Errorf("write idle progress: %w", writeErr)
				}
				nextFailure = time.Now().Add(time.Minute)
			}
			delay = backoff
			backoff = min(2*backoff, 30*time.Second)
		} else {
			lastFailure = nil
			backoff = interval
			if !busy {
				return ctx.Err()
			}
			if !time.Now().Before(nextProgress) {
				elapsed := time.Since(start).Truncate(time.Second)
				var writeErr error
				if known {
					_, writeErr = fmt.Fprintf(opt.output, "Waiting for %s queue: preparing=%d running=%d branch=%d critical=%d (tag=%d promotion=%d plan=%d apply=%d other=%d) elapsed=%s\n", opt.mode, state.Preparing, state.Running, state.Branch, state.Critical(), state.Tag, state.Promotion, state.Plan, state.Apply, state.Other, elapsed)
				} else {
					_, writeErr = fmt.Fprintf(opt.output, "Waiting for %s queue: busy (counts unavailable on this server) elapsed=%s\n", opt.mode, elapsed)
				}
				if writeErr != nil {
					return fmt.Errorf("write idle progress: %w", writeErr)
				}
				nextProgress = time.Now().Add(15 * time.Second)
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return finish(ctx.Err())
		case <-timer.C:
		}
	}
}

// probeIdle keeps protocol errors separate from retryable read failures. A
// failed observation never establishes idleness, even after a previous success.
func probeIdle(ctx context.Context, client *http.Client, target, mode string) (state model.IdleQueueStatus, busy, known, transient bool, err error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil) //nolint:gosec // G704: production uses a fixed admin Unix-socket URL; dynamic loopback targets are test fixtures.
	if err != nil {
		return state, false, false, false, err
	}
	response, err := client.Do(request) //nolint:gosec // G704: the production transport always dials the fixed private Unix socket.
	if err != nil {
		return state, false, false, true, fmt.Errorf("read live daemon queue: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusNoContent, http.StatusConflict:
		if mode == "critical" && response.Header.Get("X-Oberth-Idle-Version") != "1" {
			return state, false, false, false, errors.New("server does not support critical queue observation; upgrade its wait-idle support first or use --wait-for=all")
		}
		if response.StatusCode == http.StatusNoContent {
			return state, false, false, false, nil
		}
	default:
		retry := response.StatusCode >= 500 && response.StatusCode <= 599
		if retry {
			// Small daemon error bodies can be drained so the Unix-socket
			// connection is reused; oversized/stalled bodies stay bounded.
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4097))
		}
		return state, false, false, retry, fmt.Errorf("live daemon queue unavailable (HTTP %d)", response.StatusCode)
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
	if readErr != nil {
		return state, false, false, true, fmt.Errorf("read live daemon queue: %w", readErr)
	}
	known = len(body) > 0
	if (mode == "critical" && !known) || (known && (len(body) > 4096 || recordjson.Unmarshal(body, &state) != nil || !state.Valid())) {
		return state, false, false, false, errors.New("invalid live daemon queue snapshot")
	}
	return state, true, known, false, nil
}
