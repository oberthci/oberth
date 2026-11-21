package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/term"
)

func terminalOutput(out io.Writer) io.Writer {
	file, ok := out.(interface{ Fd() uintptr })
	if ok && term.IsTerminal(int(file.Fd())) {
		return &terminalWriter{out: out}
	}
	return out
}

// terminalWriter preserves SGR, tabs and newlines; other controls are visible.
// Pending bytes allow colour and UTF-8 sequences to span log pages or writes.
type terminalWriter struct {
	out     io.Writer
	pending []byte
}

func (w *terminalWriter) Write(p []byte) (int, error) {
	if err := w.write(p, false); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (w *terminalWriter) Flush() error { return w.write(nil, true) }

func (w *terminalWriter) write(p []byte, final bool) error {
	data := append(w.pending, p...)
	w.pending = nil
	var out strings.Builder
	for len(data) > 0 {
		n, incomplete := sgrPrefix(data)
		if !final && (incomplete || !utf8.FullRune(data)) {
			w.pending = append([]byte(nil), data...)
			break
		}
		if n > 0 {
			out.Write(data[:n])
			data = data[n:]
			continue
		}
		size := writeTerminalRune(&out, data)
		data = data[size:]
	}
	n, err := io.WriteString(w.out, out.String())
	if err == nil && n != out.Len() {
		err = io.ErrShortWrite
	}
	return err
}

func sgrPrefix(data []byte) (int, bool) {
	if data[0] != '\x1b' {
		return 0, false
	}
	if len(data) == 1 {
		return 0, true
	}
	if data[1] != '[' {
		return 0, false
	}
	for i := 2; i < len(data) && i < 128; i++ {
		b := data[i]
		if b == 'm' {
			return i + 1, false
		}
		if b != ';' && b != ':' && (b < '0' || b > '9') {
			return 0, false
		}
	}
	return 0, len(data) < 128
}

func writeTerminalRune(out *strings.Builder, data []byte) int {
	r, size := utf8.DecodeRune(data)
	if r == utf8.RuneError && size == 1 {
		_, _ = fmt.Fprintf(out, "\\x%02x", data[0])
	} else if r != '\n' && r != '\t' && (unicode.IsControl(r) || unicode.Is(unicode.Cf, r)) {
		quoted := strconv.QuoteRuneToASCII(r)
		out.WriteString(quoted[1 : len(quoted)-1])
	} else {
		out.WriteRune(r)
	}
	return size
}
