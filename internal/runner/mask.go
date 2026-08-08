package runner

import (
	"bytes"
	"io"
	"sort"
	"strings"
	"sync"
)

// MaskReplacement is written in place of every supplied secret value.
const MaskReplacement = "***"

const maxBufferedLineBytes = 64 << 10

// Masker redacts exact known secret values and non-empty lines from multiline
// secret values. Longer values are replaced first.
type Masker struct {
	secrets [][]byte
	maxLen  int
}

// NewMasker builds an immutable masker.
func NewMasker(values []string) Masker {
	unique := make(map[string]struct{})
	for _, value := range values {
		addSecret := func(candidate string) {
			if candidate != "" {
				unique[candidate] = struct{}{}
			}
		}
		addSecret(value)
		addSecret(strings.TrimRight(value, "\r\n"))
		normalizedLines := strings.ReplaceAll(value, "\r\n", "\n")
		normalizedLines = strings.ReplaceAll(normalizedLines, "\r", "\n")
		for _, line := range strings.Split(normalizedLines, "\n") {
			addSecret(line)
		}
	}
	ordered := make([]string, 0, len(unique))
	for secret := range unique {
		ordered = append(ordered, secret)
	}
	sort.Slice(ordered, func(left, right int) bool {
		if len(ordered[left]) == len(ordered[right]) {
			return ordered[left] < ordered[right]
		}
		return len(ordered[left]) > len(ordered[right])
	})
	masker := Masker{secrets: make([][]byte, 0, len(ordered))}
	for _, secret := range ordered {
		masker.secrets = append(masker.secrets, []byte(secret))
		masker.maxLen = max(masker.maxLen, len(secret))
	}
	return masker
}

// Mask returns text with every known secret value redacted.
func (m Masker) Mask(value string) string {
	return string(m.maskBytes([]byte(value)))
}

func (m Masker) maskBytes(value []byte) []byte {
	masked := bytes.Clone(value)
	for _, secret := range m.secrets {
		masked = bytes.ReplaceAll(masked, secret, []byte(MaskReplacement))
	}
	return masked
}

type prefixedWriter struct {
	mu        sync.Mutex
	out       io.Writer
	prefix    []byte
	masker    Masker
	pending   []byte
	lineStart bool
	afterCR   bool
	err       error
}

func newPrefixedWriter(out io.Writer, prefix string, masker Masker) *prefixedWriter {
	return &prefixedWriter{out: out, prefix: []byte(prefix), masker: masker, lineStart: true}
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return 0, w.err
	}
	w.pending = append(w.pending, p...)
	if w.afterCR && len(w.pending) > 0 {
		if w.pending[0] == '\n' {
			w.pending = w.pending[1:]
		}
		w.afterCR = false
	}
	for {
		delimiter := bytes.IndexAny(w.pending, "\r\n")
		if delimiter < 0 {
			break
		}
		delimiterByte := w.pending[delimiter]
		if err := w.emitLocked(w.pending[:delimiter], true); err != nil {
			w.err = err
			return 0, err
		}
		w.pending = w.pending[delimiter+1:]
		if delimiterByte == '\r' {
			if len(w.pending) == 0 {
				w.afterCR = true
			} else if w.pending[0] == '\n' {
				w.pending = w.pending[1:]
			}
		}
	}
	for len(w.pending) > maxBufferedLineBytes+w.masker.maxLen {
		cut := w.safeCutLocked(len(w.pending) - max(0, w.masker.maxLen-1))
		if cut <= 0 {
			break
		}
		if err := w.emitLocked(w.pending[:cut], false); err != nil {
			w.err = err
			return 0, err
		}
		w.pending = w.pending[cut:]
	}
	return len(p), nil
}

func (w *prefixedWriter) safeCutLocked(cut int) int {
	for _, secret := range w.masker.secrets {
		searchStart := max(0, cut-len(secret)+1)
		for offset := searchStart; offset < cut; {
			match := bytes.Index(w.pending[offset:], secret)
			if match < 0 {
				break
			}
			start := offset + match
			if start >= cut {
				break
			}
			if start+len(secret) > cut {
				cut = start
				break
			}
			offset = start + 1
		}
	}
	return cut
}

func (w *prefixedWriter) Flush() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	if len(w.pending) == 0 {
		return nil
	}
	err := w.emitLocked(w.pending, true)
	w.pending = nil
	if err != nil {
		w.err = err
	}
	return err
}

func (w *prefixedWriter) emitLocked(value []byte, newline bool) error {
	if w.lineStart {
		if _, err := w.out.Write(w.prefix); err != nil {
			return err
		}
		w.lineStart = false
	}
	if len(value) > 0 {
		if _, err := w.out.Write(w.masker.maskBytes(value)); err != nil {
			return err
		}
	}
	if newline {
		if _, err := io.WriteString(w.out, "\n"); err != nil {
			return err
		}
		w.lineStart = true
	}
	return nil
}
