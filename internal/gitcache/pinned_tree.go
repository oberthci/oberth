package gitcache

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strconv"
	"strings"
)

type pinnedTreeLimit struct {
	writer    io.Writer
	remaining int64
}

func (w *pinnedTreeLimit) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("pinned source exceeds byte limit")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}

// ExportPinnedTree reads exact cached blob objects, not git archive's optional
// export-ignore/export-subst projection. No .git directory or credentials are
// copied. Symlinks and submodules refuse rather than silently losing content.
func (c *Cache) ExportPinnedTree(ctx context.Context, input, sha, destination string, limit int64) error {
	if err := ValidateSHA(sha); err != nil {
		return err
	}
	if limit < 1 || limit > 128<<20 {
		return errors.New("invalid pinned source byte limit")
	}
	_, repoPath, err := c.path(input)
	if err != nil {
		return err
	}
	lock := c.repoLock(repoPath)
	if err := lock.RLockContext(ctx); err != nil {
		return err
	}
	defer lock.RUnlock()
	if err := c.checkRepositoryActive(ctx, input); err != nil {
		return err
	}
	if err := c.assertCommit(ctx, repoPath, sha); err != nil {
		return err
	}
	kind, err := c.capture(ctx, repoPath, "cat-file", "-t", sha)
	if err != nil || strings.TrimSpace(kind) != "commit" {
		return errors.New("pinned source object is not an exact commit")
	}
	var inventory bytes.Buffer
	if err := c.run(ctx, commandSpec{dir: repoPath, args: []string{"ls-tree", "-r", "-l", "-z", sha}, stdout: &pinnedTreeLimit{writer: &inventory, remaining: 1 << 20}}); err != nil {
		return fmt.Errorf("read pinned tree inventory: %w", err)
	}
	entries := bytes.Split(bytes.TrimSuffix(inventory.Bytes(), []byte{0}), []byte{0})
	if inventory.Len() == 0 {
		entries = nil
	}
	if len(entries) > 4096 {
		return errors.New("pinned source exceeds 4096 files")
	}
	if err := os.Mkdir(destination, 0700); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	root, err := os.OpenRoot(destination)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	var total int64
	for _, entry := range entries {
		meta, name, ok := strings.Cut(string(entry), "\t")
		fields := strings.Fields(meta)
		if !ok || len(fields) != 4 || fields[1] != "blob" || (fields[0] != "100644" && fields[0] != "100755") || ValidateSHA(fields[2]) != nil || name == "." || path.Clean(name) != name || strings.HasPrefix(name, "/") || strings.HasPrefix(name, "../") || strings.Contains(name, "\\") {
			return errors.New("pinned source contains an unsupported tree entry")
		}
		for _, segment := range strings.Split(name, "/") {
			if strings.EqualFold(segment, ".git") {
				return errors.New("pinned source contains a Git metadata path")
			}
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil || size < 0 || size > limit-total {
			return errors.New("pinned source exceeds byte limit")
		}
		total += size
		if err := root.MkdirAll(path.Dir(name), 0755); err != nil {
			return err
		}
		mode := os.FileMode(0644)
		if fields[0] == "100755" {
			mode = 0755
		}
		file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return err
		}
		bounded := &pinnedTreeLimit{writer: file, remaining: size}
		copyErr := c.run(ctx, commandSpec{dir: repoPath, args: []string{"cat-file", "blob", fields[2]}, stdout: bounded})
		closeErr := file.Close()
		if err := errors.Join(copyErr, closeErr); err != nil {
			return err
		}
		if bounded.remaining != 0 {
			return errors.New("pinned source blob length mismatch")
		}
	}
	complete = true
	return nil
}
