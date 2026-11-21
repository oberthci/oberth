package gitcache

import (
	"archive/tar"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/gitport"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

func (w *archiveWriter) history(ctx context.Context) error {
	gitdir := filepath.Join(w.scratch, "git")
	if err := os.MkdirAll(gitdir, 0700); err != nil {
		return err
	}
	b, c := w.binding, w.cache
	format := "sha1"
	if len(b.Revision.Commit) == 64 {
		format = "sha256"
	}
	_, err := c.capture(
		ctx,
		command{dir: gitdir, args: []string{"init", "--bare", "--object-format=" + format, "."}},
		1<<20,
	)
	if err != nil {
		return err
	}
	refs := append([]sourcecontract.Ref{b.Export.Ref, b.Export.DefaultBranch}, b.Export.Tags...)
	roots := []string{b.Revision.Commit}
	for _, r := range refs {
		if r.Name != "" {
			if err = c.object(ctx, w.store, r.ObjectSHA); err != nil {
				if isAbsent(err) && strings.HasPrefix(r.Name, "refs/tags/") {
					return &tagMovedError{err}
				}
				return err
			}
			roots = append(roots, r.ObjectSHA)
		}
	}
	if err = c.pack(ctx, w.store, gitdir, roots); err != nil {
		return err
	}
	if err = c.snapshotRefs(ctx, gitdir, refs); err != nil {
		return err
	}
	if err = c.finishHistory(ctx, gitdir, b.Revision.Commit); err != nil {
		return err
	}
	w.total, err = archiveMetadata(ctx, w.tar, gitdir, w.total)
	return errors.Join(err, os.RemoveAll(gitdir))
}

func (c *Cache) pack(ctx context.Context, p, dest string, roots []string) error {
	slices.Sort(roots)
	roots = slices.Compact(roots)
	pack, err := os.OpenFile(filepath.Join(dest, "objects", "pack", "source.pack"), os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer func() { _ = pack.Close() }()
	err = c.run(
		ctx,
		command{
			dir:    p,
			args:   []string{"pack-objects", "--stdout", "--revs"},
			input:  strings.NewReader(strings.Join(roots, "\n") + "\n"),
			output: &limitedWriter{writer: pack, left: PackBytes},
		},
	)
	if err != nil {
		return err
	}
	return c.run(ctx, command{dir: dest, args: []string{"index-pack", "objects/pack/source.pack"}})
}
func archiveMetadata(ctx context.Context, tw *tar.Writer, root string, total int64) (int64, error) {
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		if d.IsDir() || !metadataFile(rel) {
			return nil
		}
		n, err := writeArchiveFile(ctx, tw, archiveEntry{p, ".git/" + rel, MainBytes - total})
		total += n
		return err
	})
	return total, err
}
func metadataFile(rel string) bool {
	return rel == "config" ||
		rel == "HEAD" ||
		rel == "index" ||
		strings.HasPrefix(rel, "refs/") ||
		strings.HasPrefix(rel, "objects/pack/")
}

type archiveEntry struct {
	path, name string
	budget     int64
}

func writeArchiveFile(ctx context.Context, tw *tar.Writer, entry archiveEntry) (int64, error) {
	p, name, budget := entry.path, entry.name, entry.budget
	f, err := os.Open(p)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, refuse("metadata mode")
	}
	if info.Size() > budget {
		return 0, &LimitError{Limit: MainBytes}
	}
	if err = tw.WriteHeader(&tar.Header{Name: name, Mode: 0444, Size: info.Size(), Typeflag: tar.TypeReg}); err != nil {
		return 0, err
	}
	return io.Copy(tw, &contextReader{ctx: ctx, reader: f})
}

func (c *Cache) object(ctx context.Context, p, oid string) error {
	data, err := c.capture(
		ctx,
		command{
			dir:   p,
			args:  []string{"cat-file", "--batch-check=%(objecttype)"},
			input: strings.NewReader(oid + "\n"),
		},
		128,
	)
	if err != nil {
		return err
	}
	if string(data) == oid+" missing" {
		return &gitport.AbsentError{Ref: oid}
	}
	return nil
}
func repositoryConfig(format string, bare bool) []byte {
	text := "[core]\n bare=false\n"
	if bare {
		text = "[core]\n bare=true\n"
	}
	if format == "sha256" {
		text += " repositoryFormatVersion=1\n[extensions]\n objectFormat=sha256\n"
	}
	return []byte(text)
}

func (c *Cache) snapshotRefs(ctx context.Context, p string, refs []sourcecontract.Ref) error {
	values := make(map[string]string)
	for _, r := range refs {
		if r.Name != "" {
			values[r.Name] = r.ObjectSHA
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	slices.Sort(names)
	for len(names) > 0 {
		batch := names[:min(len(names), MaxUpdates)]
		var tx strings.Builder
		for _, name := range batch {
			fmt.Fprintf(&tx, "update %s %s\n", name, values[name])
		}
		if err := c.run(
			ctx,
			command{
				dir:   p,
				args:  []string{"update-ref", "--stdin"},
				input: strings.NewReader(tx.String()), refWrites: batch,
			},
		); err != nil {
			return err
		}
		names = names[len(batch):]
	}
	return nil
}

func (c *Cache) finishHistory(ctx context.Context, p, commit string) error {
	if err := os.WriteFile(filepath.Join(p, "HEAD"), []byte(commit+"\n"), 0600); err != nil {
		return err
	}
	if err := c.run(ctx, command{dir: p, args: []string{"read-tree", commit}}); err != nil {
		return err
	}
	if len(commit) == 64 {
		return os.WriteFile(filepath.Join(p, "config"), repositoryConfig("sha256", false), 0600)
	}
	return os.Remove(filepath.Join(p, "config"))
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if r.ctx.Err() != nil {
		return 0, context.Cause(r.ctx)
	}
	return r.reader.Read(p)
}
