package store

import "context"

// Historical schema tests deliberately exercise the pre-startup migration path.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	return open(ctx, path, opts, true, nil)
}
