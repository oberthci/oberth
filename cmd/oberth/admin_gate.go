package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const defaultAdminSocket = "/tmp/oberth-admin.sock"
const maximumAdminGateError = 4 << 10

func newAdminGateServer(gate func(context.Context) error, databasePath string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/mutation-gate", func(output http.ResponseWriter, request *http.Request) {
		if gate == nil {
			http.Error(output, "audit mutation gate is unavailable", http.StatusServiceUnavailable)
			return
		}
		if request.ContentLength > 0 {
			http.Error(output, "request body is not accepted", http.StatusBadRequest)
			return
		}
		if request.Header.Get("X-Oberth-Admin-Database") != databasePath {
			http.Error(output, "requested database is not the live daemon database", http.StatusConflict)
			return
		}
		if err := gate(request.Context()); err != nil {
			http.Error(output, err.Error(), http.StatusServiceUnavailable)
			return
		}
		output.WriteHeader(http.StatusNoContent)
	})
	return &http.Server{
		Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10,
	}
}

func requestAdminMutationGate(ctx context.Context, socket, operation, databasePath string) error {
	if err := validateAdminSocket(socket); err != nil {
		return err
	}
	if strings.TrimSpace(operation) == "" || strings.ContainsAny(operation, "\x00\r\n") {
		return errors.New("admin mutation operation is invalid")
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 35 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://oberth.local/v1/mutation-gate", nil)
	if err != nil {
		return fmt.Errorf("build admin mutation-gate request: %w", err)
	}
	request.Header.Set("X-Oberth-Admin-Operation", operation)
	request.Header.Set("X-Oberth-Admin-Database", databasePath)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request daemon audit mutation gate for %s: %w", operation, err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError+1))
	if readErr != nil {
		return fmt.Errorf("read daemon audit mutation-gate rejection for %s: %w", operation, readErr)
	}
	if len(body) > maximumAdminGateError {
		return fmt.Errorf("daemon audit mutation gate rejected %s with an oversized response", operation)
	}
	reason := strings.TrimSpace(string(body))
	if reason == "" {
		reason = response.Status
	}
	return fmt.Errorf("daemon audit mutation gate rejected %s: %s", operation, reason)
}

func requestLiveAdminMutationGate(ctx context.Context, operation, databasePath string) error {
	return requestAdminMutationGate(ctx, defaultAdminSocket, operation, databasePath)
}

func listenAdminSocket(ctx context.Context, path string) (net.Listener, error) {
	if err := validateAdminSocket(path); err != nil {
		return nil, err
	}
	if err := removeStaleAdminSocket(path); err != nil {
		return nil, err
	}
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen admin socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = listener.Close()
		_ = removeStaleAdminSocket(path)
		return nil, fmt.Errorf("restrict admin socket permissions: %w", err)
	}
	return listener, nil
}

func validateAdminSocket(path string) error {
	clean := filepath.Clean(path)
	if path != clean || !filepath.IsAbs(path) || filepath.Dir(path) != "/tmp" || filepath.Base(path) == "." || len(path) > 100 {
		return errors.New("admin socket must be a clean direct child of /tmp no longer than 100 bytes")
	}
	return nil
}

func removeStaleAdminSocket(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect admin socket: %w", err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("refuse to replace non-socket admin path %s", path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale admin socket: %w", err)
	}
	return nil
}
