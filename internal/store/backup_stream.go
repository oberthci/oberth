package store

import (
	"context"
	"errors"
	"io"
	"sync"
)

// OpenVerifiedBackup prepares one verified snapshot beside the live database.
// The archive streams directly from that snapshot without a second disk copy.
func OpenVerifiedBackup(ctx context.Context, path string, expected int, opts Options) (io.ReadCloser, error) {
	prepared, err := prepareVerifiedBackup(ctx, path, expected, opts, false)
	if err != nil {
		return nil, err
	}
	reader, writer := io.Pipe()
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { _ = reader.CloseWithError(ctx.Err()) })
	go func() {
		defer close(done)
		defer stop()
		err := prepared.write(ctx, writer)
		err = errors.Join(err, prepared.cleanup())
		_ = writer.CloseWithError(err)
	}()
	return &backupStream{PipeReader: reader, done: done}, nil
}

type backupStream struct {
	*io.PipeReader
	done <-chan struct{}
	once sync.Once
	err  error
}

func (s *backupStream) Close() error {
	s.once.Do(func() { s.err = s.PipeReader.Close(); <-s.done })
	return s.err
}
