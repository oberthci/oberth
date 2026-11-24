package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"sync"
)

//nolint:gochecknoglobals // Writers are exclusively borrowed for one response and released for reuse or GC.
var jsonGzipWriters = sync.Pool{New: func() any { return gzip.NewWriter(io.Discard) }}

func gzipJSON(data []byte) []byte {
	var buf bytes.Buffer
	z := jsonGzipWriters.Get().(*gzip.Writer)
	z.Reset(&buf)
	_, _ = z.Write(data)
	_ = z.Close()
	// The pooled writer must not retain the response buffer.
	z.Reset(io.Discard)
	jsonGzipWriters.Put(z)
	return buf.Bytes()
}
