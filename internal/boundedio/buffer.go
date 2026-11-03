// Package boundedio provides a rejecting bound for subprocess output.
package boundedio

import "errors"

// Buffer retains at most Limit bytes. Overflow rejects the whole write, invokes
// OnOverflow once, and makes the error sticky. A caller may cancel its producer
// in OnOverflow. Like bytes.Buffer, it requires external synchronization.
type Buffer struct {
	Limit      int
	LimitError error
	OnOverflow func()
	data       []byte
	err        error
}

func (b *Buffer) Write(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	if len(p) > b.Limit-len(b.data) {
		b.err = b.LimitError
		if b.err == nil {
			b.err = errors.New("output exceeds byte limit")
		}
		if b.OnOverflow != nil {
			b.OnOverflow()
		}
		return 0, b.err
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
func (b *Buffer) Bytes() []byte  { return b.data }
func (b *Buffer) String() string { return string(b.data) }
func (b *Buffer) Err() error     { return b.err }
