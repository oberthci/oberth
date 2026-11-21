package gitcache

import (
	"io"
	"strconv"
)

// Receive advertises and reports status in pkt-lines. Bands two and three carry
// native diagnostics; band one and unbanded protocol packets pass unchanged.
type receiveOutput struct {
	next   io.Writer
	frame  []byte
	need   int
	detail nativeDetail
}

func (w *receiveOutput) Write(p []byte) (int, error) {
	consumed := 0
	for len(p) > 0 {
		if w.need == 0 {
			w.need = 4
		}
		n := min(w.need-len(w.frame), len(p))
		w.frame = append(w.frame, p[:n]...)
		p, consumed = p[n:], consumed+n
		if len(w.frame) < w.need {
			continue
		}
		if len(w.frame) == 4 {
			size, err := strconv.ParseUint(string(w.frame), 16, 16)
			if err != nil || size == 3 {
				return consumed, io.ErrUnexpectedEOF
			}
			w.need = max(4, int(size))
			if w.need > 4 {
				continue
			}
		}
		if err := w.emit(); err != nil {
			return consumed, err
		}
		w.frame, w.need = w.frame[:0], 4
	}
	return consumed, nil
}

func (w *receiveOutput) emit() error {
	if len(w.frame) > 4 && (w.frame[4] == 2 || w.frame[4] == 3) {
		_, err := w.detail.Write(w.frame[5:])
		return err
	}
	if w.next == nil {
		return nil
	}
	n, err := w.next.Write(w.frame)
	if err == nil && n != len(w.frame) {
		return io.ErrShortWrite
	}
	return err
}
