// Package storetest builds isolated fixtures through production database openers.
package storetest

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/oberthci/oberth/internal/store"
)

type fixtureInfo struct {
	path string
	now  func() time.Time
}

var fixtures sync.Map

func Open(ctx context.Context, path string, opts store.Options) (*store.Store, error) {
	database, err := store.CreateGenesis(ctx, path, opts)
	if errors.Is(err, os.ErrExist) {
		inspection, inspectErr := store.InspectCurrent(ctx, path, opts)
		if inspectErr != nil {
			return nil, inspectErr
		}
		_, verifyErr := inspection.VerifyStartupAuditState(ctx)
		closeErr := inspection.Close()
		if err := errors.Join(verifyErr, closeErr); err != nil {
			return nil, err
		}
		database, err = store.OpenCurrent(ctx, path, opts)
	}
	if err != nil {
		return nil, err
	}
	if _, err = database.ReconcileInconsistentPublications(ctx); err == nil {
		err = database.RecoverOwnerState(ctx)
	}
	if err != nil {
		return nil, errors.Join(err, database.Close())
	}
	return Track(database, path, opts), nil
}

// Track attaches explicit seed helpers to a handle opened directly through a
// production opener, for startup and backup fixtures that must test that opener.
func Track(database *store.Store, path string, opts store.Options) *store.Store {
	fixtures.Store(database, fixtureInfo{path: path, now: opts.Now})
	return database
}

// OpenAdminClient opens path through store.OpenAdminClient, first creating a
// genesis database when none exists, as a running server would have. The
// production opener refuses a missing database.
func OpenAdminClient(ctx context.Context, path string, opts store.Options) (*store.Store, error) {
	if err := EnsureDatabase(ctx, path, opts); err != nil {
		return nil, err
	}
	return store.OpenAdminClient(ctx, path, opts)
}

// EnsureDatabase creates a genesis database at path unless one exists.
func EnsureDatabase(ctx context.Context, path string, opts store.Options) error {
	base, _, _ := strings.Cut(path, "?")
	if _, err := os.Stat(base); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	database, err := store.CreateGenesis(ctx, base, opts)
	if err != nil {
		return err
	}
	return database.Close()
}
