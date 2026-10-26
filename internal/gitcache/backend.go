package gitcache

import (
	"context"
	"fmt"
)

// Backend selects the implementation of the cache's validated Git operations.
// The CLI backend remains available during the in-process backend transition.
type Backend string

const BackendCLI Backend = "cli"

// gitBackend is the boundary between repository policy and Git plumbing. The
// request vocabulary is private and constructed only by this package; it is
// not a shell or a public arbitrary-command API. Both backends must preserve
// streaming, bounded capture, cancellation and policy error semantics.
type gitBackend interface {
	execute(context.Context, commandSpec, bool) (string, error)
}

type cliBackend struct{ cache *Cache }

func (c *Cache) initBackend(selected Backend) error {
	switch selected {
	case "", BackendCLI:
		c.backend = &cliBackend{cache: c}
		c.backendName = BackendCLI
		return nil
	default:
		return fmt.Errorf("unknown Git backend %q", selected)
	}
}

func (c *Cache) execute(ctx context.Context, spec commandSpec, capture bool) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	return c.backend.execute(ctx, spec, capture)
}
