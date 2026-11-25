package secretstore

import (
	"context"
	"fmt"
	"io"
	"net/http"
)

// RestoreSnapshot applies a checked snapshot to an explicitly initialized target.
// The temporary target's administrator authorizes replacing its seal state.
func (c *Client) RestoreSnapshot(ctx context.Context, token []byte, in io.ReadSeeker, limit int64) error {
	if len(token) == 0 || in == nil || InspectSnapshot(in, limit) != nil {
		return refuse("input", nil)
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return refuse("input", nil)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.address+"/v1/sys/storage/raft/snapshot-force", io.LimitReader(in, limit+1))
	if err != nil {
		return refuse("address", nil)
	}
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Vault-Token", string(token))
	client := *c.http
	client.Timeout = 0 // The explicit restore owns the deadline.
	response, err := client.Do(request)
	if err != nil {
		return refuse("connect", ctxCause(ctx, err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent && response.StatusCode != http.StatusOK {
		return refuse("request", fmt.Errorf("HTTP %d", response.StatusCode))
	}
	return nil
}
