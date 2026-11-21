package boundedio

import (
	"bytes"
	"fmt"
)

// LimitError reports a refused write/add. No prefix of a refused input is retained.
type LimitError struct{ Limit int64 }

func (e *LimitError) Reason() string {
	return fmt.Sprintf("Output exceeds its %d-byte limit.", e.Limit)
}
func (*LimitError) Remedy() string {
	return "Reduce the output or increase the caller's explicit byte limit."
}
func (e *LimitError) Error() string { return e.Reason() + " " + e.Remedy() }

// Buffer retains bounded bytes. Overflow is sticky; the producer adapter owns
// cancellation and joins. Buffer performs no callbacks or downstream writes.
// Like bytes.Buffer it requires external synchronization.
type Buffer struct {
	limit    int
	data     []byte
	overflow bool
}

// New clamps a negative byte limit to zero.
func New(limit int) *Buffer { return &Buffer{limit: max(0, limit)} }

func (b *Buffer) Write(input []byte) (int, error) {
	if b.overflow || len(input) > b.limit-len(b.data) {
		b.overflow = true
		return 0, &LimitError{Limit: int64(b.limit)}
	}
	b.data = append(b.data, input...)
	return len(input), nil
}

func (b *Buffer) Bytes() []byte { return bytes.Clone(b.data) }
func (b *Buffer) Len() int      { return len(b.data) }
func (b *Buffer) Err() error {
	if b.overflow {
		return &LimitError{Limit: int64(b.limit)}
	}
	return nil
}
