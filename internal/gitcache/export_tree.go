package gitcache

import (
	"archive/tar"
	"bufio"
	"context"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/oberthci/oberth/internal/sourcecontract"
	"github.com/oberthci/oberth/internal/sourcedelivery"
)

type treeEntry struct {
	path, oid string
	mode      int64
	size      int64
}

// LimitError is a permanent source-size refusal, including pack overflow.
type LimitError struct {
	Limit int64
	Unit  string
}

func (e *LimitError) Error() string { return e.Reason() + ". " + e.Remedy() }
func (*LimitError) Reason() string  { return (&sourcedelivery.Failure{Code: "too_large"}).Error() }
func (*LimitError) Remedy() string  { return (&sourcedelivery.Failure{Code: "too_large"}).Remedy() }

type limitedWriter struct {
	writer io.Writer
	left   int64
	limit  int64
	err    error
}

func (l *limitedWriter) Write(b []byte) (int, error) {
	if l.limit == 0 {
		l.limit = l.left
	}
	if int64(len(b)) > l.left {
		l.err = &LimitError{Limit: l.limit}
		return 0, l.err
	}
	n, err := l.writer.Write(b)
	l.left -= int64(n)
	return n, err
}
func parseEntry(record []byte) (treeEntry, error) {
	h, p, ok := strings.Cut(string(record), "\t")
	f := strings.Fields(h)
	if !ok || len(f) != 4 {
		return treeEntry{}, refuse("tree metadata")
	}
	mode, err := strconv.ParseInt(f[0], 8, 32)
	if err != nil {
		return treeEntry{}, err
	}
	size := int64(0)
	if f[3] != "-" {
		size, err = strconv.ParseInt(f[3], 10, 64)
	}
	if err != nil || size < 0 {
		return treeEntry{}, refuse("tree size")
	}
	return treeEntry{path: p, oid: f[2], mode: mode, size: size}, nil
}

type archiveWriter struct {
	cache          *Cache
	tar            *tar.Writer
	store, scratch string
	binding        sourcecontract.Binding
	limit, total   int64
	files          int
}

func (w *archiveWriter) tree(ctx context.Context) error {
	inventory, err := os.CreateTemp(w.scratch, "tree-")
	if err != nil {
		return err
	}
	defer func() { _ = inventory.Close(); _ = os.Remove(inventory.Name()) }()
	err = w.cache.run(
		ctx,
		command{
			dir:    w.store,
			args:   []string{"ls-tree", "-r", "-l", "-z", w.binding.Revision.Commit},
			output: &limitedWriter{writer: inventory, left: MetadataBytes},
		},
	)
	if err != nil {
		return err
	}
	if _, err = inventory.Seek(0, 0); err != nil {
		return err
	}
	return w.cache.batch(ctx, w.store, func(input io.Writer, output io.Reader) error {
		return w.writeTree(ctx, bufio.NewReader(inventory), input, bufio.NewReader(output))
	})
}
func (
	w *archiveWriter,
) writeTree(
	ctx context.Context,
	reader *bufio.Reader,
	input io.Writer,
	output *bufio.Reader,
) error {
	for {
		record, e := reader.ReadBytes(0)
		if errors.Is(e, io.EOF) && len(record) == 0 {
			return nil
		}
		if e != nil {
			return e
		}
		entry, e := parseEntry(record[:len(record)-1])
		if e != nil {
			return e
		}
		if e = w.entry(ctx, entry, batchObjects{input, output}); e != nil {
			return e
		}
	}
}
func (w *archiveWriter) entry(ctx context.Context, entry treeEntry, batch batchObjects) error {
	pinned := w.binding.Descriptor.Kind == sourcecontract.Pinned
	if err := validateTreePath(entry.path); err != nil {
		if pinned {
			return err
		}
		return nil
	}
	if entry.mode != 0100644 && entry.mode != 0100755 {
		if pinned {
			return refuse("pinned link")
		}
		return nil
	}
	w.files++
	if pinned && w.files > PinFiles {
		return &LimitError{Limit: PinFiles, Unit: "files"}
	}
	if entry.size > w.limit-w.total {
		return &LimitError{Limit: w.limit}
	}
	mode := int64(0444)
	if entry.mode == 0100755 {
		mode = 0555
	}
	if err := w.tar.WriteHeader(
		&tar.Header{Name: entry.path, Mode: mode, Size: entry.size, Typeflag: tar.TypeReg},
	); err != nil {
		return err
	}
	if err := batch.copy(ctx, entry, w.tar); err != nil {
		return err
	}
	w.total += entry.size
	return nil
}

func validateTreePath(path string) error {
	// Backslash is a literal filename byte on the server filesystem.
	return sourcecontract.ValidateFilePath(strings.ReplaceAll(path, "\\", "_"))
}
