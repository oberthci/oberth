package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

// Readiness remains observable through Kubernetes exec when an unready pod has
// been removed from the public Service's endpoints. The existing owner-only
// admin socket carries the same cached, secret-free state as /readyz.
func withAdminReadiness(server *http.Server, ready func(context.Context) error) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/readiness", func(output http.ResponseWriter, request *http.Request) {
		response := model.Readiness(ready(request.Context()))
		output.Header().Set("Content-Type", "application/json")
		output.Header().Set("Cache-Control", "no-store")
		if response.Status != "ready" {
			output.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(output).Encode(response)
	})
	mux.Handle("/", server.Handler)
	server.Handler = mux
	return server
}

func adminReadiness(ctx context.Context, socket string, output io.Writer) error {
	if err := validateAdminSocket(socket); err != nil {
		return err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://oberth.local/v1/readiness", nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return errors.New("daemon readiness is unavailable")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusServiceUnavailable {
		return errors.New("daemon readiness is unavailable")
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError+1))
	var state model.ReadinessResponse
	if err != nil || len(raw) > maximumAdminGateError || json.Unmarshal(raw, &state) != nil || !state.Valid() || (state.Status == "ready") != (response.StatusCode == http.StatusOK) {
		return errors.New("invalid daemon readiness response")
	}
	return json.NewEncoder(output).Encode(state)
}
