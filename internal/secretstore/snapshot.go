package secretstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// BackupRole is bound only to the installation's dedicated backup account.
const BackupRole = "oberth-backup"

type SnapshotRequest struct {
	Config Config
	JWT    []byte
	Limit  int64
}

// SnapshotAvailable checks the dedicated login without requiring root authority.
// Existing stores may not have the backup role yet. Other failures remain errors.
func (c *Client) SnapshotAvailable(ctx context.Context, request SnapshotRequest) (bool, error) {
	if len(request.JWT) == 0 || len(request.JWT) > 64<<10 || !validName(request.Config.AuthMount) {
		return false, refuse("input", nil)
	}
	var result struct {
		Auth struct {
			Token string `json:"client_token"`
		} `json:"auth"`
	}
	status, err := c.JSON(ctx, Request{Method: "POST", Path: "/auth/" + request.Config.AuthMount + "/login", Body: map[string]string{"jwt": string(request.JWT), "role": BackupRole}}, &result)
	if status == http.StatusBadRequest || status == http.StatusForbidden || status == http.StatusNotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if result.Auth.Token == "" {
		return false, refuse("response", nil)
	}
	token := []byte(result.Auth.Token)
	defer clear(token)
	return true, joinRevoke(ctx, c, token, nil)
}

// Snapshot streams the native Raft snapshot under a short-lived read-only login.
// The caller owns the total deadline and encrypts the output as it arrives.
func (c *Client) Snapshot(ctx context.Context, request SnapshotRequest, out io.Writer) (err error) {
	cfg, jwt, limit := request.Config, request.JWT, request.Limit
	if out == nil || limit <= 0 {
		return refuse("input", nil)
	}
	token, err := c.login(ctx, jwt, cfg.AuthMount, BackupRole)
	if err != nil {
		return err
	}
	defer clear(token)
	defer func() { err = joinRevoke(ctx, c, token, err) }()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.address+"/v1/sys/storage/raft/snapshot", nil)
	if err != nil {
		return refuse("address", nil)
	}
	req.Header.Set("X-Vault-Token", string(token))
	client := *c.http
	client.Timeout = 0 // The backup operation supplies its bounded lifetime.
	response, err := client.Do(req)
	if err != nil {
		return refuse("connect", ctx.Err())
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return refuse("request", fmt.Errorf("HTTP %d", response.StatusCode))
	}
	n, err := io.Copy(out, io.LimitReader(response.Body, limit+1))
	if err != nil || n == 0 || n > limit {
		return refuse("limit", nil)
	}
	return nil
}
