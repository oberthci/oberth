package main

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/oberthci/oberth/internal/publicerr"
)

type boundedWriter struct {
	ctx       context.Context
	writer    io.Writer
	remaining int64
	progress  func()
}

func (w *boundedWriter) Write(body []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(body)) > w.remaining {
		return 0, recoveryError(archiveTooLarge, nil)
	}
	n, err := w.writer.Write(body)
	noteProgress(w.progress, n)
	w.remaining -= int64(n)
	return n, errors.Join(err, w.ctx.Err())
}

type contextReader struct {
	ctx context.Context
	io.Reader
	progress func()
}

func (r contextReader) Read(body []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := r.Reader.Read(body)
	noteProgress(r.progress, n)
	return n, err
}

func authenticatedStreamError(err error) error {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var path *os.PathError
	var public *publicerr.Error
	if errors.As(err, &path) || errors.As(err, &public) {
		return err
	}
	return recoveryError(damagedEncrypted, err)
}

func noteProgress(touch func(), n int) {
	if touch != nil && n > 0 {
		touch()
	}
}
