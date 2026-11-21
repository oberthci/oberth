package releaseverify

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"io/fs"
)

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	if len(p) > 1<<20 {
		p = p[:1<<20]
	}
	return r.reader.Read(p)
}

type checkedReader struct {
	file          fs.File
	ctx           context.Context
	sum           hash.Hash
	want          string
	length, count int64
	max           int64
	err           error
	closed        bool
}

func (r *checkedReader) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	if r.closed {
		return 0, fs.ErrClosed
	}
	n, err := (&contextReader{ctx: r.ctx, reader: io.LimitReader(r.file, r.max-r.count+1)}).Read(p)
	_, _ = r.sum.Write(p[:n])
	r.count += int64(n)
	if r.count > r.max {
		err = Limit
	} else if r.mismatch(err) {
		err = Digest
	} else if err != nil && !errors.Is(err, io.EOF) && r.ctx.Err() == nil {
		err = readFailure(err)
	}
	if err != nil && !errors.Is(err, io.EOF) {
		clear(p[:n])
		n = 0
	}
	r.err = err
	return n, err
}
func (r *checkedReader) Close() error {
	if !r.closed {
		_, err := io.Copy(io.Discard, r)
		closeErr := r.file.Close()
		r.closed = true
		if err != nil {
			r.err = err
		} else {
			r.err = closeErr
		}
	}
	if errors.Is(r.err, io.EOF) {
		return nil
	}
	return r.err
}

func (r *checkedReader) mismatch(err error) bool {
	if r.length >= 0 && (r.count > r.length || errors.Is(err, io.EOF) && r.count != r.length) {
		return true
	}
	return (errors.Is(err, io.EOF) || r.length >= 0 && r.count == r.length) && fmt.Sprintf("%x", r.sum.Sum(nil)) != r.want
}
