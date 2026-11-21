package client

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/oberthci/oberth/internal/apicontract"
)

// DatabaseBackup opens the independently authorized canonical database stream.
// The caller supplies its deadline, verifies the archive and closes the body.
func (c *Client) DatabaseBackup(ctx context.Context) (io.ReadCloser, error) {
	address := strings.TrimSuffix(c.endpoint, "/mcp") + "/api/admin/database-backup"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, address, strings.NewReader("{}"))
	if err != nil {
		return nil, failure("url", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/octet-stream")
	response, err := c.authenticatedRequest(req)
	if err != nil {
		return nil, requestFailure(apicontract.Definition{}, err)
	}
	media, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode == http.StatusOK && mediaErr == nil && media == "application/octet-stream" {
		return response.Body, nil
	}
	defer response.Body.Close()
	if response.StatusCode >= 400 && media == "application/json" {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, apicontract.AdminBytes+1))
		if readErr == nil && len(body) <= apicontract.AdminBytes {
			return nil, resultError(body)
		}
	}
	return nil, failure("reply", nil)
}
