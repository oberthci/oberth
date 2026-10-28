package gitcache

import (
	"context"
)

// gitBackend is the boundary between repository policy and Git plumbing. The
// request vocabulary is private and constructed only by this package; it is
// not a shell or a public arbitrary-command API. The CLI must preserve
// streaming, bounded capture, cancellation and policy error semantics.
type gitBackend interface {
	execute(context.Context, commandSpec, bool) (string, error)
}

type cliBackend struct{ cache *Cache }

func (c *Cache) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.backend.execute(ctx, spec, capture)
}
