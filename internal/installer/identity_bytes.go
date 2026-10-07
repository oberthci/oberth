package installer

// Byte-only decoding for OpenBao identity bundles (issue #811).
//
// readData decodes `bao read -format=json` into map[string]any, which turns
// every value — including the server TLS private key, the SSH host key, and
// Rekor database passwords — into a Go string. Strings cannot be zeroed, so
// those copies outlive their use for as long as the installer process (and
// its freed pages) do. The helpers here keep secret-bearing reads as byte
// slices from the exec stream to the consumer: the raw stdout buffer is the
// caller's to clear, each field is unescaped into its own []byte, and the
// bundle clears all of them in one call. Nothing in this path converts a
// value to a string.
//
// Known residuals (documented, not fixed here): exec.Cmd.CombinedOutput
// grows its buffer by reallocation, so earlier, smaller copies of the output
// may remain until reused; parsed private keys (*rsa.PrivateKey,
// *ecdsa.PrivateKey, ssh.Signer) hold big.Int limbs that Go cannot clear.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf16"
	"unicode/utf8"
)

// readRaw returns the raw `bao read -format=json` output for path, or nil
// when the path holds no value. Unlike readData it never decodes values: the
// returned buffer is the only copy of any secret the read produced, and the
// caller must clear() it as soon as it has been decoded. Only the CLI's
// explicit "No value found" absence is tolerated; every other failure
// propagates.
func (b openBaoExec) readRaw(ctx context.Context, token, path string) ([]byte, error) {
	out, err := b.authenticated(ctx, token, nil, "read", "-format=json", path)
	if err != nil {
		if bytes.Contains(out, []byte("No value found")) {
			clear(out)
			return nil, nil
		}
		return nil, fmt.Errorf("bao read %s: %w%s", path, err, commandOutputSuffix(out))
	}
	return out, nil
}

// writeRaw writes a caller-built JSON body to path over stdin. Unlike
// writeJSON it never marshals a map, which would hold any secret value as a
// Go string; the caller owns body and clears it after the call.
func (b openBaoExec) writeRaw(ctx context.Context, token, path string, body []byte) error {
	if out, err := b.authenticated(ctx, token, body, "write", path, "-"); err != nil {
		return fmt.Errorf("bao write %s: %w%s", path, err, commandOutputSuffix(out))
	}
	return nil
}

// identityBundle is one KV v2 identity bundle decoded straight into byte
// slices. Every value is a fresh allocation owned by the bundle; clear()
// zeroes all of them.
type identityBundle struct {
	data    map[string][]byte
	version int
}

// field returns the value stored under name, or nil when the bundle is nil
// or the field is absent. Callers must not retain the slice past clear().
func (b *identityBundle) field(name string) []byte {
	if b == nil {
		return nil
	}
	return b.data[name]
}

// clear zeroes every value and empties the map. Safe on a nil bundle.
func (b *identityBundle) clear() {
	if b == nil {
		return
	}
	for name, value := range b.data {
		clear(value)
		delete(b.data, name)
	}
}

// decodeIdentityBundle decodes the `bao read -format=json` output of a KV v2
// data path into an identityBundle. The outermost JSON object is located the
// way parseBaoJSON locates it (stray warning lines around it are tolerated).
// Field values are captured as json.RawMessage — a byte copy of the escaped
// literal, never a string — then unescaped into their own []byte; the literal
// copy is cleared before return. A JSON null field is treated as absent.
// Errors never carry output bytes.
func decodeIdentityBundle(raw []byte) (*identityBundle, error) {
	start := bytes.IndexByte(raw, '{')
	end := bytes.LastIndexByte(raw, '}')
	if start < 0 || end < start {
		return nil, errors.New("no JSON object in bao output")
	}
	var envelope struct {
		Data struct {
			Data     map[string]json.RawMessage `json:"data"`
			Metadata struct {
				Version int `json:"version"`
			} `json:"metadata"`
		} `json:"data"`
	}
	err := json.Unmarshal(raw[start:end+1], &envelope)
	bundle := &identityBundle{
		data:    make(map[string][]byte, len(envelope.Data.Data)),
		version: envelope.Data.Metadata.Version,
	}
	for name, literal := range envelope.Data.Data {
		if err != nil {
			clear(literal)
			continue
		}
		value, unquoteErr := unquoteJSONBytes(literal)
		clear(literal)
		if unquoteErr != nil {
			err = fmt.Errorf("identity field %q: %w", name, unquoteErr)
			continue
		}
		if value != nil {
			bundle.data[name] = value
		}
	}
	if err != nil {
		bundle.clear()
		return nil, fmt.Errorf("parse bao identity JSON: %w", err)
	}
	return bundle, nil
}

// unquoteJSONBytes decodes one JSON string literal, quotes included, into
// UTF-8 bytes. It is encoding/json's string decoding without the string: the
// result is a fresh []byte the caller can clear. A JSON null yields nil, nil.
// Invalid escapes, raw control characters, and unpaired surrogates are
// errors rather than U+FFFD replacements, so a damaged identity never
// decodes to bytes other than the ones that were stored.
func unquoteJSONBytes(literal []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(literal)
	if bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	if len(trimmed) < 2 || trimmed[0] != '"' || trimmed[len(trimmed)-1] != '"' {
		return nil, errors.New("value is not a JSON string")
	}
	body := trimmed[1 : len(trimmed)-1]
	out := make([]byte, 0, len(body))
	fail := func(msg string) ([]byte, error) {
		clear(out)
		return nil, errors.New(msg)
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if c != '\\' {
			if c < 0x20 {
				return fail("unescaped control character in JSON string")
			}
			out = append(out, c)
			continue
		}
		i++
		if i >= len(body) {
			return fail("truncated escape in JSON string")
		}
		switch body[i] {
		case '"', '\\', '/':
			out = append(out, body[i])
		case 'b':
			out = append(out, '\b')
		case 'f':
			out = append(out, '\f')
		case 'n':
			out = append(out, '\n')
		case 'r':
			out = append(out, '\r')
		case 't':
			out = append(out, '\t')
		case 'u':
			r, consumed, err := decodeUnicodeEscape(body[i-1:])
			if err != nil {
				clear(out)
				return nil, err
			}
			out = utf8.AppendRune(out, r)
			// The loop's own increment accounts for one byte; the escape
			// started one byte before i.
			i += consumed - 2
		default:
			return fail("invalid escape in JSON string")
		}
	}
	return out, nil
}

// decodeUnicodeEscape decodes a \uXXXX escape at the start of b, pairing a
// high surrogate with the \uXXXX that must follow it. It returns the rune and
// the number of bytes consumed (6, or 12 for a surrogate pair).
func decodeUnicodeEscape(b []byte) (rune, int, error) {
	r1, ok := hex4(b)
	if !ok {
		return 0, 0, errors.New("invalid \\u escape in JSON string")
	}
	if !utf16.IsSurrogate(r1) {
		return r1, 6, nil
	}
	if r1 >= 0xDC00 {
		return 0, 0, errors.New("unpaired low surrogate in JSON string")
	}
	r2, ok := hex4(b[6:])
	if !ok || r2 < 0xDC00 || r2 > 0xDFFF {
		return 0, 0, errors.New("unpaired high surrogate in JSON string")
	}
	return utf16.DecodeRune(r1, r2), 12, nil
}

// hex4 reads a \uXXXX escape at the start of b.
func hex4(b []byte) (rune, bool) {
	if len(b) < 6 || b[0] != '\\' || b[1] != 'u' {
		return 0, false
	}
	var r rune
	for _, c := range b[2:6] {
		var v byte
		switch {
		case '0' <= c && c <= '9':
			v = c - '0'
		case 'a' <= c && c <= 'f':
			v = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			v = c - 'A' + 10
		default:
			return 0, false
		}
		r = r<<4 | rune(v)
	}
	return r, true
}
