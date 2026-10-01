package redact

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"slices"
)

var ErrClosed = errors.New("redacting writer is closed")

// minEncodedLen is the minimum length of an encoded pattern to register.
// Patterns shorter than this risk false-positive redactions.
const minEncodedLen = 16

// minFragmentLen is the minimum byte length a line-split fragment of a
// multiline secret must reach before it is registered as a separate
// redaction pattern. Shorter fragments from multiline secrets with very
// short individual lines risk false-positive masking of ordinary build
// output. The full original value is always registered regardless of length.
const minFragmentLen = 8

type Writer struct {
	destination io.Writer
	secrets     [][]byte
	buffer      []byte
	closed      bool
}

func NewWriter(destination io.Writer, values [][]byte) *Writer {
	secrets := make([][]byte, 0, len(values))
	seen := map[string]bool{}
	addSecret := func(value []byte) {
		if len(value) == 0 || seen[string(value)] {
			return
		}
		copyValue := slices.Clone(value)
		secrets = append(secrets, copyValue)
		seen[string(copyValue)] = true
	}

	// Phase 1: register exact, trimmed, and line-split raw patterns.
	var rawPatterns [][]byte
	addRaw := func(value []byte) {
		if len(value) == 0 || seen[string(value)] {
			return
		}
		rawPatterns = append(rawPatterns, slices.Clone(value))
		addSecret(value)
	}
	for _, value := range values {
		addRaw(value)
		addRaw(bytes.TrimRight(value, "\r\n"))
		normalized := bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
		for _, line := range bytes.Split(normalized, []byte("\n")) {
			// Individual line fragments from multiline secrets must meet
			// the minimum length to avoid false-positive masking of short
			// common strings. The full value is always registered above.
			if len(line) >= minFragmentLen {
				addRaw(line)
			}
		}
	}

	// Phase 2: register encoded variants (base64, hex, JSON-escaped)
	// of each raw pattern.
	for _, raw := range rawPatterns {
		registerEncodedVariants(addSecret, raw)
	}

	slices.SortFunc(secrets, func(left, right []byte) int { return len(right) - len(left) })
	return &Writer{destination: destination, secrets: secrets}
}

func (writer *Writer) Write(body []byte) (int, error) {
	if writer.closed {
		return 0, ErrClosed
	}
	want := len(body)
	writer.buffer = append(writer.buffer, body...)
	if err := writer.drain(false); err != nil {
		return 0, err
	}
	return want, nil
}

func (writer *Writer) Flush() error {
	if writer.closed {
		return ErrClosed
	}
	return writer.drain(true)
}

func (writer *Writer) Close() error {
	if writer.closed {
		return nil
	}
	err := writer.Flush()
	writer.closed = true
	return err
}

func (writer *Writer) drain(final bool) error {
	for len(writer.buffer) > 0 {
		matchIndex, matchLength := writer.firstMatch()
		if matchIndex >= 0 {
			if err := writeAll(writer.destination, writer.buffer[:matchIndex]); err != nil {
				return err
			}
			if err := writeAll(writer.destination, []byte("***")); err != nil {
				return err
			}
			writer.buffer = writer.buffer[matchIndex+matchLength:]
			continue
		}
		retain := 0
		if !final {
			retain = writer.partialSuffixLength()
		}
		emit := len(writer.buffer) - retain
		if err := writeAll(writer.destination, writer.buffer[:emit]); err != nil {
			return err
		}
		writer.buffer = slices.Clone(writer.buffer[emit:])
		return nil
	}
	return nil
}

func (writer *Writer) firstMatch() (int, int) {
	bestIndex, bestEnd := -1, 0
	for _, secret := range writer.secrets {
		index := bytes.Index(writer.buffer, secret)
		if index < 0 {
			continue
		}
		end := index + len(secret)
		if bestIndex < 0 {
			bestIndex, bestEnd = index, end
		} else if index < bestIndex {
			// Earlier start found. If the previous region overlaps
			// (its start falls before this match's end), merge them
			// by keeping the farthest end. Otherwise take the earlier
			// match alone — it will be emitted first.
			if bestIndex < end {
				bestIndex = index
				if end < bestEnd {
					// keep bestEnd (already farther)
				} else {
					bestEnd = end
				}
			} else {
				bestIndex, bestEnd = index, end
			}
		} else if index < bestEnd && end > bestEnd {
			// Starts within the current region; extend redaction.
			bestEnd = end
		}
	}
	if bestIndex < 0 {
		return -1, 0
	}
	// Fixed-point: the initial pass may have extended the region past
	// secrets it already visited. Re-scan until no secret extends the
	// region further. This catches chained overlaps where A overlaps B
	// and B overlaps C, but A does not overlap C directly — a pattern
	// processing order that visits C before B misses C on the first pass.
	for {
		grown := false
		for _, secret := range writer.secrets {
			index := bytes.Index(writer.buffer, secret)
			if index < 0 {
				continue
			}
			end := index + len(secret)
			if index >= bestIndex && index < bestEnd && end > bestEnd {
				bestEnd = end
				grown = true
			}
		}
		if !grown {
			break
		}
	}
	return bestIndex, bestEnd - bestIndex
}

func (writer *Writer) partialSuffixLength() int {
	best := 0
	for _, secret := range writer.secrets {
		limit := min(len(writer.buffer), len(secret)-1)
		for length := limit; length > best; length-- {
			if bytes.Equal(writer.buffer[len(writer.buffer)-length:], secret[:length]) {
				best = length
				break
			}
		}
	}
	return best
}

func writeAll(destination io.Writer, body []byte) error {
	for len(body) > 0 {
		written, err := destination.Write(body)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return nil
}

// base64Encodings lists the standard base64 alphabets.
var base64Encodings = []*base64.Encoding{
	base64.StdEncoding,
	base64.RawStdEncoding,
	base64.URLEncoding,
	base64.RawURLEncoding,
}

// registerEncodedVariants adds base64 (phase-aware), hex, and JSON-escaped
// representations of value to the secret set. For base64, three alignment
// phases per alphabet cover every offset a secret may occupy inside a
// concatenated encoding (e.g. base64("_json_key:" + secret) shifts the
// encoding by len("_json_key:") mod 3 = 1 byte).
func registerEncodedVariants(addSecret func([]byte), value []byte) {
	if len(value) == 0 {
		return
	}

	// Phase r in {0,1,2}: prepend r zero bytes, encode, then strip the
	// leading characters that blend with the foreign prefix (r=0: 0,
	// r=1: 2, r=2: 3) and the trailing character that blends with
	// following data or padding.
	leadStrip := [3]int{0, 2, 3}
	for _, enc := range base64Encodings {
		for r := 0; r < 3; r++ {
			input := make([]byte, r+len(value))
			copy(input[r:], value)
			encoded := enc.EncodeToString(input)

			lead := leadStrip[r]
			if lead >= len(encoded) {
				continue
			}
			core := encoded[lead:]
			// Strip padding added by padded encodings.
			for len(core) > 0 && core[len(core)-1] == '=' {
				core = core[:len(core)-1]
			}
			// Strip the last data character when the total length is not
			// a multiple of 3: that character's low bits come from the
			// next (unknown) byte.
			if (r+len(value))%3 != 0 && len(core) > 0 {
				core = core[:len(core)-1]
			}
			if len(core) >= minEncodedLen {
				addSecret([]byte(core))
			}
		}
	}

	// Hex: lowercase and uppercase.
	hexLower := hex.EncodeToString(value)
	if len(hexLower) >= minEncodedLen {
		addSecret([]byte(hexLower))
		addSecret(bytes.ToUpper([]byte(hexLower)))
	}

	// JSON-escaped: catches multi-line values (PEM, key JSON) embedded
	// in structured JSON log output.
	jsonBytes, err := json.Marshal(string(value))
	if err == nil && len(jsonBytes) > 2 {
		escaped := jsonBytes[1 : len(jsonBytes)-1]
		if !bytes.Equal(escaped, value) && len(escaped) >= minEncodedLen {
			addSecret(escaped)
		}
	}
}
