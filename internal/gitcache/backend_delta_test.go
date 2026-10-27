package gitcache

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/packfile"
)

func TestStreamingDeltaBackwardCopies(t *testing.T) {
	// Copy [8:12], rewind to [0:4], then move forwards to [20:24]. After
	// reopening the base reader its position must be zero, not the old 12.
	// Both decoder outputs have the same length; a tar size check cannot help.
	base := &plumbing.MemoryObject{}
	base.SetType(plumbing.BlobObject)
	_, _ = base.Write([]byte("0123456789abcdefghijklmnopqrstuv"))
	delta := []byte{32, 12, 0x91, 8, 4, 0x90, 4, 0x91, 20, 4}
	expected := []byte("89ab0123klmn")
	memory, err := packfile.PatchDelta([]byte("0123456789abcdefghijklmnopqrstuv"), delta)
	if err != nil || !bytes.Equal(memory, expected) {
		t.Fatalf("memory decoder: %q %v", memory, err)
	}
	reader, err := packfile.ReaderFromDelta(base, bytes.NewReader(delta))
	if err != nil {
		t.Fatal(err)
	}
	actual, err := io.ReadAll(reader)
	closeErr := reader.Close()
	if err != nil || closeErr != nil || !bytes.Equal(actual, expected) {
		t.Fatalf("streaming decoder: %q %v %v; want %q", actual, err, closeErr, expected)
	}
}

func TestDecodedObjectIntegrity(t *testing.T) {
	for _, body := range []string{"expected bytes", "corrupt! bytes", "short", "expected bytes too long"} {
		t.Run(body, func(t *testing.T) {
			object := &plumbing.MemoryObject{}
			object.SetType(plumbing.BlobObject)
			_, _ = object.Write([]byte(body))
			object.SetSize(int64(len("expected bytes")))
			checked := verifiedObject{EncodedObject: object, expected: plumbing.ComputeHash(plumbing.BlobObject, []byte("expected bytes"))}
			reader, err := checked.Reader()
			if err != nil {
				t.Fatal(err)
			}
			_, err = io.Copy(io.Discard, reader)
			_ = reader.Close()
			if body == "expected bytes" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), "git object integrity failure") {
				t.Fatalf("corrupt object accepted: %v", err)
			}
		})
	}
}

// This used to panic in ReaderFromDelta's goroutine: the failed reopen
// assigned nil to baseRd, then its deferred closure dereferenced that nil.
func TestStreamingDeltaBackwardCopiesReopenFailure(t *testing.T) {
	base := &failingReopenObject{MemoryObject: &plumbing.MemoryObject{}}
	base.SetType(plumbing.BlobObject)
	_, _ = base.Write([]byte("0123456789abcdefghijklmnopqrstuv"))
	delta := []byte{32, 8, 0x91, 8, 4, 0x90, 4}
	reader, err := packfile.ReaderFromDelta(base, bytes.NewReader(delta))
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(reader)
	if closeErr := reader.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if !errors.Is(err, packfile.ErrInvalidDelta) || string(body) != "89ab" {
		t.Fatalf("reopen failure: body=%q err=%v", body, err)
	}
	if base.opens.Load() != 2 || base.closes.Load() != 1 {
		t.Fatalf("reader lifecycle: opens=%d closes=%d", base.opens.Load(), base.closes.Load())
	}
}

type failingReopenObject struct {
	*plumbing.MemoryObject
	opens  atomic.Int32
	closes atomic.Int32
}

func (o *failingReopenObject) Reader() (io.ReadCloser, error) {
	if o.opens.Add(1) == 2 {
		return nil, errors.New("fixture base cannot be reopened")
	}
	reader, err := o.MemoryObject.Reader()
	if err != nil {
		return nil, err
	}
	return &countedDeltaBaseReader{ReadCloser: reader, closes: &o.closes}, nil
}

type countedDeltaBaseReader struct {
	io.ReadCloser
	closes *atomic.Int32
}

func (r *countedDeltaBaseReader) Close() error { r.closes.Add(1); return r.ReadCloser.Close() }
