package gitcache

import (
	"fmt"
	"io"

	"github.com/go-git/go-git/v5/plumbing"
)

// Verify decoded bytes, rather than trusting the pack index's claimed hash.
// This covers both archive delivery and worktree checkout. A same-size decoder
// error must fail the operation so a partial workspace is removed, never seeded.
func (s *atomicReferenceStore) EncodedObject(kind plumbing.ObjectType, hash plumbing.Hash) (plumbing.EncodedObject, error) {
	object, err := s.Storage.EncodedObject(kind, hash)
	if err != nil {
		return nil, err
	}
	return verifiedObject{EncodedObject: object, expected: hash}, nil
}

type verifiedObject struct {
	plumbing.EncodedObject
	expected plumbing.Hash
}

func (o verifiedObject) Reader() (io.ReadCloser, error) {
	reader, err := o.EncodedObject.Reader()
	if err != nil {
		return nil, err
	}
	return &verifiedObjectReader{ReadCloser: reader, hasher: plumbing.NewHasher(o.Type(), o.Size()), expected: o.expected, size: o.Size()}, nil
}

type verifiedObjectReader struct {
	io.ReadCloser
	hasher   plumbing.Hasher
	expected plumbing.Hash
	size     int64
	read     int64
	failure  error
}

func (r *verifiedObjectReader) Read(body []byte) (int, error) {
	if r.failure != nil {
		return 0, r.failure
	}
	n, err := r.ReadCloser.Read(body)
	_, _ = r.hasher.Write(body[:n])
	r.read += int64(n)
	if err == io.EOF {
		actual := r.hasher.Sum()
		if r.read != r.size || actual != r.expected {
			r.failure = fmt.Errorf("git object integrity failure: object=%s decoded=%s size=%d expected_size=%d", r.expected, actual, r.read, r.size)
			return n, r.failure
		}
	}
	return n, err
}
