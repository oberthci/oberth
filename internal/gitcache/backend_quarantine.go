package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"

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
	active     *os.File
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
	active, err := lockReceiveQuarantine(directory, false)
	if err != nil {
		_ = os.RemoveAll(directory)
		return nil, err
	}
	store := filesystem.NewStorageWithOptions(osfs.New(directory), cache.NewObjectLRU(32<<20), filesystem.Options{LargeObjectThreshold: 1, AlternatesFS: osfs.New("/")})
	cleanup := func() { _ = store.Close(); _ = os.RemoveAll(directory); _ = active.Close() }
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
	return &receiveQuarantine{repository: repository, store: atomic, live: live, active: active}, nil
}
func (q *receiveQuarantine) close() error {
	return errors.Join(q.store.Close(), os.RemoveAll(q.store.dir), q.active.Close())
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

// Interrupted receives are collectible after 30 minutes. A held kernel lock
// protects even a very old active receive, including across server processes;
// process exit releases it automatically without PID reuse or lease guessing.
const receiveQuarantineMaxAge = 30 * time.Minute

func lockReceiveQuarantine(directory string, nonblocking bool) (*os.File, error) {
	// #nosec G304 -- directory is a repository-owned MkdirTemp receive directory or its same-prefix sweep entry; the fixed marker refuses symlinks.
	file, err := os.OpenFile(filepath.Join(directory, ".oberth-active"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	operation := unix.LOCK_EX
	if nonblocking {
		operation |= unix.LOCK_NB
	}
	if err := unix.Flock(int(file.Fd()), operation); err != nil {
		_ = file.Close()
		return nil, err
	}
	return file, nil
}

func sweepReceiveQuarantines(repository string, now time.Time) error {
	objects := filepath.Join(repository, "objects")
	entries, err := os.ReadDir(objects)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var failures error
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "oberth-incoming-") {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		if now.Sub(info.ModTime()) < receiveQuarantineMaxAge {
			continue
		}
		directory := filepath.Join(objects, entry.Name())
		_, markerErr := os.Lstat(filepath.Join(directory, ".oberth-active"))
		legacy := errors.Is(markerErr, os.ErrNotExist)
		active, err := lockReceiveQuarantine(directory, true)
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			failures = errors.Join(failures, err)
			continue
		}
		// Recheck under the lock: a receive or another sweeper may have changed it.
		candidate := info
		info, err = os.Lstat(directory)
		// Older releases had no lock marker. Creating it changes the directory
		// mtime; retain the original age only for that legacy directory inode.
		if err == nil && info.IsDir() && os.SameFile(candidate, info) && (legacy || now.Sub(info.ModTime()) >= receiveQuarantineMaxAge) {
			err = os.RemoveAll(directory)
		}
		failures = errors.Join(failures, err, active.Close())
	}
	return failures
}
