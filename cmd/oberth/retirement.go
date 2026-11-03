package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

func withLegacyRemovalCheck(server *http.Server, path string, check func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/", server.Handler)
	mux.HandleFunc("GET /v1/retirement-check", func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || r.Header.Get("X-Oberth-Admin-Database") != path {
			http.Error(w, "retirement check does not match live daemon", http.StatusConflict)
			return
		}
		if check == nil {
			http.Error(w, "retirement check unavailable", http.StatusServiceUnavailable)
			return
		}
		// Only this read-only inventory needs a longer response budget. Keep
		// the admin server's normal timeout on all other routes and requests.
		// A writer without deadline support retains its existing server limit.
		if err := http.NewResponseController(w).SetWriteDeadline(time.Now().Add(3 * time.Minute)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			http.Error(w, "retirement check deadline unavailable", http.StatusServiceUnavailable)
			return
		}
		if err := check(r.Context()); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		w.Header().Set("X-Oberth-Retirement-Version", "1")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusNoContent)
	})
	server.Handler = mux
	return server
}

func runRetirementCheck(ctx context.Context, arguments []string, output io.Writer) error {
	flags := flag.NewFlagSet("admin retirement-check", flag.ContinueOnError)
	flags.SetOutput(output)
	path := flags.String("database", "/data/oberth.sqlite", "live daemon database path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return fmt.Errorf("%w: admin retirement-check [--database path]", errUsage)
	}
	if err := requestRetirementCheck(ctx, defaultAdminSocket, *path); err != nil {
		return err
	}
	_, err := fmt.Fprintln(output, `{"format":"oberth-retirement-check-v1","ready":true}`)
	return err
}

func requestRetirementCheck(ctx context.Context, socket, path string) error {
	if err := validateAdminSocket(socket); err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://oberth.local/v1/retirement-check", nil)
	if err != nil {
		return err
	}
	request.Header.Set("X-Oberth-Admin-Database", path)
	response, err := (&http.Client{Transport: transport, Timeout: 3 * time.Minute}).Do(request)
	if err != nil {
		return fmt.Errorf("request daemon retirement check: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent && response.Header.Get("X-Oberth-Retirement-Version") == "1" {
		return nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError+1))
	if err != nil || len(body) > maximumAdminGateError {
		return errors.New("daemon retirement check returned an unreadable or oversized refusal")
	}
	return fmt.Errorf("daemon retirement check refused: %s: %s", response.Status, strings.TrimSpace(string(body)))
}
