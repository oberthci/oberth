package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/oberthci/oberth/internal/model"
)

func requestLiveUplinkTokenPolicy(ctx context.Context, databasePath string) (model.UplinkTokenPolicy, error) {
	return requestUplinkTokenPolicy(ctx, defaultAdminSocket, databasePath)
}

// Issuance runs in a separate in-pod process. Read policy from the live daemon
// over its UID-protected socket instead of assuming this CLI's defaults.
func requestUplinkTokenPolicy(ctx context.Context, socket, databasePath string) (model.UplinkTokenPolicy, error) {
	if err := validateAdminSocket(socket); err != nil {
		return model.UplinkTokenPolicy{}, err
	}
	dialer := net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{DisableKeepAlives: true, DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return dialer.DialContext(ctx, "unix", socket)
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://oberth.local/v1/token-policy", nil)
	if err != nil {
		return model.UplinkTokenPolicy{}, err
	}
	request.Header.Set("X-Oberth-Admin-Database", databasePath)
	response, err := client.Do(request)
	if err != nil {
		return model.UplinkTokenPolicy{}, fmt.Errorf("read live uplink token policy: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return model.UplinkTokenPolicy{}, fmt.Errorf("live uplink token policy unavailable: HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maximumAdminGateError+1))
	if err != nil {
		return model.UplinkTokenPolicy{}, err
	}
	if len(body) > maximumAdminGateError {
		return model.UplinkTokenPolicy{}, errors.New("live uplink token policy response too large")
	}
	var policy model.UplinkTokenPolicy
	if err := json.Unmarshal(body, &policy); err != nil {
		return policy, err
	}
	return policy, policy.Validate()
}

const legacyTokenWarningInterval = time.Hour

func watchNonExpiringUplinkTokens(ctx context.Context, count func(context.Context) (int, error), logger *log.Logger) {
	ticker := time.NewTicker(legacyTokenWarningInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		number, err := count(readCtx)
		cancel()
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			logger.Printf("WARNING: unable to check uplink tokens without expiry")
		} else if number > 0 {
			logger.Printf("WARNING: %d active uplink tokens have no expiry; rotate them deliberately with oberth uplink rotate", number)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
