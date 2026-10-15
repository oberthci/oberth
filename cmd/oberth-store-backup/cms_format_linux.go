//go:build linux

package main

import (
	"bytes"
	"encoding/asn1"
	"errors"
	"io"
	"os"
)

// This is only a bounded DER shape gate for our fixed OpenSSL output, not a CMS
// cryptographic implementation. RFC 5083 AuthEnvelopedData and RFC 5084 AES-GCM
// are required. OpenSSL must subsequently authenticate successfully before any
// provisional plaintext is parsed. CBC EnvelopedData is never admissible.
type derCursor struct {
	r        io.ReaderAt
	pos, end int64
}

func (c *derCursor) take(tag byte) (derCursor, error) {
	bad := errors.New("CMS DER shape refused")
	var header [10]byte
	if c.end-c.pos < 2 {
		return derCursor{}, bad
	}
	if _, err := c.r.ReadAt(header[:2], c.pos); err != nil || header[0] != tag {
		return derCursor{}, bad
	}
	head := int64(2)
	size := int64(header[1])
	if header[1]&0x80 != 0 {
		n := int64(header[1] & 0x7f)
		// The whole archive is bounded to 256 MiB, so lengths need at most 4 bytes.
		if n == 0 || n > 4 || c.end-c.pos < 2+n {
			return derCursor{}, bad
		}
		if _, err := c.r.ReadAt(header[2:2+n], c.pos+2); err != nil || header[2] == 0 {
			return derCursor{}, bad
		}
		size = 0
		for _, b := range header[2 : 2+n] {
			size = size*256 + int64(b)
		}
		if size < 128 {
			return derCursor{}, bad
		}
		head += n
	}
	if size > c.end-c.pos-head {
		return derCursor{}, bad
	}
	child := derCursor{c.r, c.pos + head, c.pos + head + size}
	c.pos = child.end
	return child, nil
}

func (c *derCursor) exact(tag byte, value []byte) error {
	v, err := c.take(tag)
	if err != nil || v.end-v.pos != int64(len(value)) {
		return errors.New("CMS field refused")
	}
	var buf [32]byte
	if len(value) > len(buf) {
		return errors.New("CMS field bound refused")
	}
	if _, err := v.r.ReadAt(buf[:len(value)], v.pos); err != nil || !bytes.Equal(buf[:len(value)], value) {
		return errors.New("CMS field refused")
	}
	return nil
}

func (c *derCursor) oid(value asn1.ObjectIdentifier) error {
	encoded, err := asn1.Marshal(value)
	if err != nil {
		return err
	}
	return c.exact(6, encoded[2:]) // All three fixed OIDs have short-form length.
}

func requireAuthenticatedCMS(path string) error {
	f, err := os.Open(path) // #nosec G304 -- exact administrator-bound ciphertext path, already regular-file and digest checked.
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 256<<20 {
		return errors.New("CMS size refused")
	}
	return checkAuthenticatedCMS(derCursor{f, 0, info.Size()})
}

func checkAuthenticatedCMS(file derCursor) error {
	bad := errors.New("authenticated CMS format refused")
	content, err := file.take(0x30)
	if err != nil || file.pos != file.end {
		return bad
	}
	if err := content.oid(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 23}); err != nil {
		return bad
	}
	explicit, err := content.take(0xa0)
	if err != nil || content.pos != content.end {
		return bad
	}
	auth, err := explicit.take(0x30)
	if err != nil || explicit.pos != explicit.end || auth.exact(2, []byte{0}) != nil {
		return bad
	}
	recipients, err := auth.take(0x31)
	if err != nil {
		return bad
	}
	recipient, err := recipients.take(0x30)
	if err != nil || recipient.pos == recipient.end || recipients.pos != recipients.end {
		return bad
	}
	encrypted, err := auth.take(0x30)
	if err != nil || encrypted.oid(asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 1}) != nil {
		return bad
	}
	algorithm, err := encrypted.take(0x30)
	if err != nil || algorithm.oid(asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 46}) != nil {
		return bad
	}
	params, err := algorithm.take(0x30)
	if err != nil || algorithm.pos != algorithm.end {
		return bad
	}
	nonce, err := params.take(4)
	if err != nil || nonce.end-nonce.pos != 12 || params.exact(2, []byte{16}) != nil || params.pos != params.end {
		return bad
	}
	cipher, err := encrypted.take(0x80)
	if err != nil || cipher.end == cipher.pos || encrypted.pos != encrypted.end {
		return bad
	}
	mac, err := auth.take(4)
	if err != nil || mac.end-mac.pos != 16 || auth.pos != auth.end {
		return bad
	}
	return nil
}
