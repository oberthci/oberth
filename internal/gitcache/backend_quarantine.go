package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/go-git/go-billy/v5/osfs"
	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

type receiveQuarantine struct {
	repository *git.Repository
	store      *atomicReferenceStore
	live       *atomicReferenceStore
}

func newReceiveQuarantine(r *git.Repository) (*receiveQuarantine, error) {
	live, ok := r.Storer.(*atomicReferenceStore)
	if !ok {
		return nil, errors.New("atomic live object store required")
	}
	directory, err := os.MkdirTemp(filepath.Join(live.dir, "objects"), "oberth-incoming-")
	if err != nil {
		return nil, err
	}
	store := filesystem.NewStorageWithOptions(osfs.New(directory), cache.NewObjectLRU(32<<20), filesystem.Options{LargeObjectThreshold: 1, AlternatesFS: osfs.New("/")})
	cleanup := func() { _ = store.Close(); _ = os.RemoveAll(directory) }
	if err := store.Init(); err != nil {
		cleanup()
		return nil, err
	}
	// This trusted, temporary alternate resolves incoming thin-pack bases. It
	// is never copied into the live repository or persisted after the receive.
	if err := store.AddAlternate(live.dir); err != nil {
		cleanup()
		return nil, err
	}
	// Policy sees the exact live refs, while every object write goes to the
	// isolated filesystem. Ref publication only happens later on the live r.
	store.ReferenceStorage = live.ReferenceStorage
	atomic := &atomicReferenceStore{Storage: store, dir: directory}
	repository, err := git.Open(atomic, nil)
	if err != nil {
		cleanup()
		return nil, err
	}
	return &receiveQuarantine{repository: repository, store: atomic, live: live}, nil
}
func (q *receiveQuarantine) close() error {
	return errors.Join(q.store.Close(), os.RemoveAll(q.store.dir))
}
func (q *receiveQuarantine) validate(ctx context.Context, roots []plumbing.Hash, known map[plumbing.Hash]bool) error {
	// fsck every received object, including objects not selected by an update.
	// IterEncodedObjects enumerates this quarantine, not its read-only alternate.
	iter, err := q.store.IterEncodedObjects(plumbing.AnyObject)
	if err != nil {
		return err
	}
	err = iter.ForEach(func(object plumbing.EncodedObject) error {
		if object.Type() != plumbing.BlobObject {
			roots = append(roots, object.Hash())
		}
		return ctx.Err()
	})
	iter.Close()
	if err != nil {
		return err
	}
	return validateGoGraph(ctx, q.repository, roots, known)
}
func (q *receiveQuarantine) promote(ctx context.Context) error {
	if err := copyLocalObjectFiles(ctx, filepath.Join(q.store.dir, "objects"), filepath.Join(q.live.dir, "objects")); err != nil {
		return err
	}
	// Earlier policy reads may have initialized the live store's pack index.
	q.live.Reindex()
	return nil
}

var _ io.Closer = (*receiveQuarantine)(nil)

func (q *receiveQuarantine) Close() error { return q.close() }
