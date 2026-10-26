package gitcache

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
	"github.com/go-git/go-git/v5/plumbing/revlist"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

const maintenanceGrace = 14 * 24 * time.Hour
const maintenanceBatchBytes = 64 << 20
const maintenanceBatchObjects = 8192

// A one-object delta window bounds pure-Go redelta CPU. Existing source packs
// remain available for the grace period; real-mirror size tradeoffs are
// measured by the shipped harness before production cutover.
const maintenanceDeltaWindow = 1
const maintenancePackThreshold = 64
const maintenanceLooseThreshold = 1024
const maintenanceStamp = "oberth-maintenance-attempt"

// RepackObjects builds one all-object encoding plan and removes reachable
// loose objects before finalizing its writer. Instead use the same go-git
// pack encoder in bounded batches, close every pack successfully, and only
// then remove aged source packs/loose objects. No aggressive run-object prune.
func (b *goGitBackend) maintain(ctx context.Context, r *git.Repository, automatic bool) error {
	original, ok := r.Storer.(*atomicReferenceStore)
	if !ok {
		return errors.New("atomic object store required")
	}
	packs, err := original.ObjectPacks()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	interval := max(maintenanceGrace, 2*b.cache.timeout)
	if automatic {
		// At most one automatic attempt per grace period bounds disk duplication
		// while old packs remain available to readers. Explicit Compact retries
		// are available for a failed attempt; background failures are logged.
		stamp := filepath.Join(original.dir, maintenanceStamp)
		if info, err := os.Lstat(stamp); err == nil {
			if !info.Mode().IsRegular() {
				return errors.New("maintenance stamp is not regular")
			}
			if now.Sub(info.ModTime()) < interval {
				return nil
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		loose := 0
		if len(packs) < maintenancePackThreshold {
			err := original.ForEachObjectHash(func(plumbing.Hash) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				loose++
				if loose >= maintenanceLooseThreshold {
					return storer.ErrStop
				}
				return nil
			})
			if err != nil {
				return err
			}
			if loose < maintenanceLooseThreshold {
				return nil
			}
		}
	}
	if err := writeMaintenanceStamp(original.dir, now); err != nil {
		return err
	}
	store := &contextObjectStore{atomicReferenceStore: original, ctx: ctx}
	roots, err := goKnownRoots(r)
	if err != nil {
		return err
	}
	wants := make([]plumbing.Hash, 0, len(roots))
	for hash := range roots {
		wants = append(wants, hash)
	}
	hashes, err := revlist.Objects(store, wants, nil)
	if err != nil {
		return err
	}
	sort.Slice(hashes, func(i, j int) bool { return hashes[i].String() < hashes[j].String() })
	// Lazily streamed objects keep the plan metadata-only. Batching also bounds
	// the delta selector's temporary content buffers on large mirrors.
	batch := []plumbing.Hash{}
	bytes := int64(0)
	newPacks := map[plumbing.Hash]bool{}
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		writer, err := original.PackfileWriter()
		if err != nil {
			return err
		}
		encoder := packfile.NewEncoder(contextWriteCloser{ctx, writer}, store, false)
		hash, encodeErr := encoder.Encode(batch, maintenanceDeltaWindow)
		closeErr := writer.Close()
		if encodeErr != nil || closeErr != nil {
			return errors.Join(encodeErr, closeErr)
		}
		newPacks[hash] = true
		batch = nil
		bytes = 0
		return nil
	}
	for _, hash := range hashes {
		if err := ctx.Err(); err != nil {
			return err
		}
		size, err := original.EncodedObjectSize(hash)
		if err != nil {
			return err
		}
		if len(batch) > 0 && (bytes+size > maintenanceBatchBytes || len(batch) >= maintenanceBatchObjects) {
			if err := flush(); err != nil {
				return err
			}
		}
		batch = append(batch, hash)
		bytes += size
	}
	if err := flush(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	cutoff := now.Add(-interval)
	for _, hash := range packs {
		if !newPacks[hash] {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := original.DeleteOldObjectPackAndIndex(hash, cutoff); err != nil {
				return err
			}
		}
	}
	packed := map[plumbing.Hash]bool{}
	for _, hash := range hashes {
		packed[hash] = true
	}
	if err := original.ForEachObjectHash(func(hash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if packed[hash] {
			return original.DeleteLooseObject(hash)
		}
		return nil
	}); err != nil {
		return err
	}
	fresh, closeStorage, err := openGoRepository(original.dir)
	if err != nil {
		return err
	}
	defer closeStorage()
	freshStore := fresh.Storer.(*atomicReferenceStore)
	fresh.Storer = &contextObjectStore{atomicReferenceStore: freshStore, ctx: ctx}
	return fresh.Prune(git.PruneOptions{OnlyObjectsOlderThan: cutoff, Handler: func(hash plumbing.Hash) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		return original.DeleteLooseObject(hash)
	}})
}
func writeMaintenanceStamp(dir string, now time.Time) error {
	file, err := os.CreateTemp(dir, ".maintenance-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() { _ = file.Close(); _ = os.Remove(temporary) }()
	if _, err := file.WriteString(now.Format(time.RFC3339Nano) + "\n"); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return os.Rename(temporary, filepath.Join(dir, maintenanceStamp))
}

type contextObjectStore struct {
	*atomicReferenceStore
	ctx context.Context
}

func (s *contextObjectStore) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	if err := s.ctx.Err(); err != nil {
		return nil, err
	}
	object, err := s.atomicReferenceStore.EncodedObject(kind, hash)
	if err != nil {
		return nil, err
	}
	return contextEncodedObject{s.ctx, object}, nil
}

type contextEncodedObject struct {
	ctx context.Context
	plumbing.EncodedObject
}

func (o contextEncodedObject) Reader() (io.ReadCloser, error) {
	reader, err := o.EncodedObject.Reader()
	if err != nil {
		return nil, err
	}
	return &contextReadCloser{contextReader{o.ctx, reader}, reader}, nil
}

type contextReadCloser struct {
	contextReader
	closer io.Closer
}

func (r *contextReadCloser) Close() error { return r.closer.Close() }

type contextWriteCloser struct {
	ctx context.Context
	io.WriteCloser
}

func (w contextWriteCloser) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.WriteCloser.Write(p)
}
