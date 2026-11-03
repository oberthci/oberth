package client

import "context"

func (client *Client) getOnce(ctx context.Context, path string, query map[string]string, into any) (error, bool) {
	return client.getOnceHeaders(ctx, path, query, into, nil)
}
