package gitcache

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Archive stages a bare-store backup and refuses active keep refs or receives.
// Its returned reader owns the private staging directory and closes it on Close.
func (c *Cache) Archive(ctx context.Context, repo string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, c.budgets.local)
	defer cancel()
	p, err := c.path(repo)
	if err != nil {
		return nil, err
	}
	if err = c.refreshRetention(ctx, p); err != nil {
		return nil, err
	}
	if err = c.lock(ctx, p); err != nil {
		return nil, err
	}
	defer c.unlock(p)
	if err = c.removable(ctx, p); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(c.config.Scratch, "oberth-export-archive-")
	if err != nil {
		return nil, err
	}
	body, err := archiveRepository(ctx, p, dir)
	if err != nil {
		_ = os.RemoveAll(dir)
	}
	return body, err
}
func (c *Cache) removable(ctx context.Context, p string) error {
	refs, err := c.keepRefs(ctx, p)
	if err != nil {
		return err
	}
	if len(refs) > 0 {
		return &RetainedError{Owner: strings.Split(refs[0], "/")[3]}
	}
	if receivePending(p) {
		return refuse("pending receive")
	}
	return nil
}
func archiveRepository(ctx context.Context, store, dir string) (io.ReadCloser, error) {
	file, err := os.OpenFile(filepath.Join(dir, "repository.tgz"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	err = filepath.WalkDir(store, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		if d.IsDir() {
			if p == filepath.Join(store, "hooks") {
				return filepath.SkipDir
			}
			return nil
		}
		name, err := filepath.Rel(store, p)
		if err != nil {
			return err
		}
		_, err = writeArchiveFile(ctx, tw, archiveEntry{p, name, 1 << 62})
		return err
	})
	if err = errors.Join(err, tw.Close(), gz.Close()); err != nil {
		return nil, err
	}
	body, err := os.Open(file.Name())
	if err != nil {
		return nil, err
	}
	return &archiveBody{File: body, dir: dir}, nil
}

type archiveBody struct {
	*os.File
	dir string
}

func (a *archiveBody) Close() error { return errors.Join(a.File.Close(), os.RemoveAll(a.dir)) }
