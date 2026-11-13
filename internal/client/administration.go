package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/recordjson"
)

// Admin calls one enumerated admin action. Mutations are never retried: an
// interrupted response may follow a committed change and needs observation.
func (client *Client) Admin(ctx context.Context, action string, arguments, into any) error {
	switch action {
	case "uplink-scope", "upstream-add", "upstream-list", "upstream-remove", "upstream-provide-key", "repo-add", "repo-verify", "access-list", "public-identity", "readiness", "version", "idle", "audit-record":
	default:
		return fmt.Errorf("client: unknown administration action")
	}
	if arguments == nil {
		arguments = struct{}{}
	}
	body, err := json.Marshal(arguments)
	if err != nil {
		return fmt.Errorf("client: encode administration arguments: %w", err)
	}
	if len(body) > api.AdminRequestBytes {
		return fmt.Errorf("client: administration arguments exceed 128 KiB")
	}
	target := *client.base
	target.Path = strings.TrimRight(target.Path, "/") + "/api/admin/" + action
	target.RawPath = ""
	target.RawQuery = ""
	target.Fragment = ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	transport := *client.http
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request) //nolint:gosec // G704: the target uses the validated configured server URL and a fixed action allowlist.
	if err != nil {
		return classifyTransport("administration", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return statusError("administration", response)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseSize+1))
	if err != nil {
		return fmt.Errorf("client: read administration response: %w", err)
	}
	if len(data) > maxResponseSize {
		return fmt.Errorf("client: administration response too large")
	}
	if into == nil {
		return nil
	}
	if err := recordjson.Unmarshal(data, into); err != nil {
		return fmt.Errorf("client: administration returned an invalid JSON response")
	}
	return nil
}

// DownloadAdminBackup streams a bounded archive. The caller must verify it
// using the database archive verifier before accepting or restoring it.
func (client *Client) DownloadAdminBackup(ctx context.Context, maxDatabaseBytes int64, output io.Writer) error {
	if maxDatabaseBytes <= 0 || maxDatabaseBytes > 4<<30 {
		return fmt.Errorf("client: backup bound must be positive and at most 4 GiB")
	}
	body, err := json.Marshal(map[string]int64{"max_database_bytes": maxDatabaseBytes})
	if err != nil {
		return err
	}
	target := *client.base
	target.Path = strings.TrimRight(target.Path, "/") + "/api/admin/database-backup"
	target.RawPath, target.RawQuery, target.Fragment = "", "", ""
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if client.token != "" {
		request.Header.Set("Authorization", "Bearer "+client.token)
	}
	transport := *client.download
	transport.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := transport.Do(request) //nolint:gosec // G704: validated configured server URL with a fixed API path, redirects refused.
	if err != nil {
		return classifyTransport("database backup", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return statusError("database backup", response)
	}
	limit := maxDatabaseBytes + (64 << 10)
	if response.ContentLength > limit {
		return fmt.Errorf("client: database backup response exceeds bound")
	}
	n, err := io.Copy(output, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return fmt.Errorf("client: read database backup: %w", err)
	}
	if n > limit {
		return fmt.Errorf("client: database backup response exceeds bound")
	}
	return nil
}
