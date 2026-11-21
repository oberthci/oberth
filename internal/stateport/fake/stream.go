package fake

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
)

// payload is private to this fake and deliberately unrelated to the real format.
func (b *Backup) payload() []byte {
	identity := fmt.Sprintf("%s\x00%s\x00%d\x00%d\x00%s\x00", b.protocol.SchemaIdentity,
		b.protocol.AuditDomain, b.report.Schema, b.report.AuditHead.ID, b.report.AuditHead.SHA256)
	digest := sha256.Sum256(append([]byte(identity), b.database...))
	return append(digest[:], b.database...)
}

type contextReader struct {
	ctx context.Context
	io.Reader
}

func (r contextReader) Read(body []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.Reader.Read(body)
}

type contextWriter struct {
	ctx context.Context
	io.Writer
}

func (w contextWriter) Write(body []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.Writer.Write(body)
}
