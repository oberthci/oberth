package service

import (
	"io"

	"github.com/oberthci/oberth/internal/job"
)

var errRunLogLimit = job.ErrRetainedLogLimit

type runLogWriter struct {
	destination io.Writer
	remaining   int64
}

func newRunLogWriter(destination io.Writer, maximum int64) io.Writer {
	return &runLogWriter{destination: destination, remaining: maximum}
}

func (writer *runLogWriter) Write(body []byte) (int, error) {
	if writer.remaining <= 0 {
		return 0, errRunLogLimit
	}
	allowed := len(body)
	limited := int64(allowed) > writer.remaining
	if limited {
		allowed = int(writer.remaining)
	}
	written, err := writer.destination.Write(body[:allowed])
	writer.remaining -= int64(written)
	if err != nil {
		return written, err
	}
	if written != allowed {
		return written, io.ErrShortWrite
	}
	if limited {
		return written, errRunLogLimit
	}
	return written, nil
}
