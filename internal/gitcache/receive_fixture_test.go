package gitcache

import (
	"context"
	"io"
)

func (c *Cache) receiveTransaction(ctx context.Context, repo, path, actor string, receive func() error, handler ReceiveHandler) error {
	// Replace only transport; preparation, gates, reservation and replay are live.
	return c.receiveWithTransport(ctx, repo, actor, "", nil, io.Discard, io.Discard, handler, receive)
}
