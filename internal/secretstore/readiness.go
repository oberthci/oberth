package secretstore

import (
	"context"
	"net/url"
)

// Readiness asks the store itself on every inspection. It reads no secret and
// sends no credential. A successful startup is not proof after a store restart.
func (c *Client) Readiness(ctx context.Context) error {
	if c == nil {
		return refuse("connect", nil)
	}
	_, err := c.JSON(ctx, Request{Method: "GET", Path: "/sys/health", Query: url.Values{"standbyok": {"true"}, "perfstandbyok": {"true"}}}, nil)
	return err
}
