package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

const defaultAdminSocket = "/run/oberth/admin.sock"
const maximumAdminGateError = 4 << 10

func newAdminGateServerWithTokenPolicy(gate func(context.Context) error, databasePath string, wake func(), policy model.UplinkTokenPolicy, counts ...func(context.Context) (model.IdleQueueStatus, error)) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/token-policy", func(output http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Oberth-Admin-Database") != databasePath {
			http.Error(output, "requested database is not the live daemon database", http.StatusConflict)
			return
		}
		if err := policy.Validate(); err != nil {
			http.Error(output, "token policy unavailable", http.StatusServiceUnavailable)
			return
		}
		output.Header().Set("Content-Type", "application/json")
		output.Header().Set("Cache-Control", "no-store")
		_ = json.NewEncoder(output).Encode(policy)
	})
	mux.HandleFunc("GET /v1/idle", func(output http.ResponseWriter, request *http.Request) {
		if len(counts) == 0 || counts[0] == nil {
			http.Error(output, "queue state unavailable", http.StatusServiceUnavailable)
			return
		}
		mode := request.URL.Query().Get("wait_for")
		if mode != "" && mode != "all" && mode != "critical" {
			http.Error(output, "invalid wait selection", http.StatusBadRequest)
			return
		}
		state, err := counts[0](request.Context())
		if err != nil {
			http.Error(output, "queue state unavailable", http.StatusServiceUnavailable)
			return
		}
		if !state.Valid() {
			http.Error(output, "queue state unavailable", http.StatusServiceUnavailable)
			return
		}
		output.Header().Set("X-Oberth-Idle-Version", "1")
		busy := state.Queued+state.Running != 0
		if mode == "critical" {
			busy = state.Critical() != 0
		}
		if busy {
			output.Header().Set("Content-Type", "application/json")
			output.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(output).Encode(state)
			return
		}
		output.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/cancellation-pass", func(output http.ResponseWriter, request *http.Request) {
		if wake == nil {
			http.Error(output, "scheduler wake is unavailable", http.StatusServiceUnavailable)
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
		wake()
		output.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/mutation-gate", func(output http.ResponseWriter, request *http.Request) {
		operations := request.Header.Values("X-Oberth-Admin-Operation")
		if len(operations) != 1 || !validAdminOperation(operations[0]) {
			http.Error(output, "admin mutation operation is invalid", http.StatusBadRequest)
			return
		}
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
	if !validAdminOperation(operation) {
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

// requestAdminSchedulerWake asks the live daemon to run its cancellation pass.
func requestAdminSchedulerWake(ctx context.Context, socket, databasePath string) error {
	if err := validateAdminSocket(socket); err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DisableKeepAlives: true,
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, "unix", socket)
		},
	}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://oberth.local/v1/cancellation-pass", nil)
	if err != nil {
		return fmt.Errorf("build scheduler wake request: %w", err)
	}
	request.Header.Set("X-Oberth-Admin-Database", databasePath)
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("wake daemon scheduler: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode == http.StatusNoContent {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError))
	reason := strings.TrimSpace(string(body))
	if reason == "" {
		reason = response.Status
	}
	return fmt.Errorf("daemon refused the scheduler wake: %s", reason)
}

func requestLiveAdminMutationGate(ctx context.Context, operation, databasePath string) error {
	return requestAdminMutationGate(ctx, defaultAdminSocket, operation, databasePath)
}

func listenAdminSocket(ctx context.Context, path string) (net.Listener, error) {
	if path == defaultAdminSocket {
		directory := filepath.Dir(path)
		if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("create admin socket directory: %w", err)
		}
		info, err := os.Lstat(directory)
		if err != nil || !info.IsDir() || !adminPathOwned(info) {
			return nil, errors.New("admin socket directory must be owned by the daemon user and must not be a symlink")
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
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
	return &adminPeerListener{Listener: listener, uid: uint32(os.Geteuid())}, nil
}

func validateAdminSocket(path string) error {
	clean := filepath.Clean(path)
	if path != clean || !filepath.IsAbs(path) || filepath.Base(path) == "." || len(path) > 100 {
		return errors.New("admin socket must be a clean absolute path no longer than 100 bytes")
	}
	info, err := os.Lstat(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("inspect admin socket directory: %w", err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 || !adminPathOwned(info) {
		return errors.New("admin socket directory must be owned by the daemon user with mode 0700 and must not be a symlink")
	}
	return nil
}

func validAdminOperation(operation string) bool {
	switch operation {
	case "upstream.database.open", "upstream.register", "upstream.remove", "upstream.identity.import",
		"repository.database.open", "repository.register", "repository.remove",
		"uplink.database.open", "uplink.register", "uplink.remove", "uplink.scope", "uplink.token.create", "uplink.token.activate",
		"access.database.open", "access.allow", "access.revoke", "audit.database.open", "audit.record",
		"kubevirt.grant", "kubevirt.revoke", "size.grant", "size.revoke", "run.cancel":
		return true
	}
	// These suffixes name OpenBao identity records, never secret material.
	for _, prefix := range []string{"upstream.identity."} {
		if suffix, ok := strings.CutPrefix(operation, prefix); ok && len(suffix) > 0 && len(suffix) <= 253 {
			for _, c := range suffix {
				if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
					return false
				}
			}
			return true
		}
	}
	return false
}

type adminPeerListener struct {
	net.Listener
	uid uint32
}

func (listener *adminPeerListener) Accept() (net.Conn, error) {
	for {
		connection, err := listener.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if err := checkAdminPeer(connection, listener.uid); err == nil {
			return connection, nil
		}
		_ = connection.Close()
	}
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
