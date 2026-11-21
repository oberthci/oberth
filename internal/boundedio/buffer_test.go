package boundedio

import (
	"errors"
	"math"
	"testing"

	"github.com/oberthci/oberth/internal/publicerr"
)

func TestBufferAtomicStickyOverflow(t *testing.T) {
	for _, limit := range []int{0, 1, 8} {
		b := New(limit)
		if n, err := b.Write(make([]byte, limit)); n != limit || err != nil {
			t.Fatal("exact limit refused")
		}
		for range 2 {
			var target *LimitError
			n, err := b.Write([]byte("x"))
			if n != 0 || !errors.As(err, &target) || b.Len() != limit || b.Err() == nil {
				t.Fatal("overflow was not atomic and sticky")
			}
		}
	}
}
func TestBytesDoNotExposeBufferStorage(t *testing.T) {
	b := New(8)
	_, _ = b.Write([]byte("abc"))
	got := b.Bytes()
	got[0] = 'z'
	if string(b.Bytes()) != "abc" {
		t.Fatal("mutable storage escaped")
	}
}
func TestNegativeLimitsClampToZero(t *testing.T) {
	for _, limit := range []int{-1, 0} {
		b := New(limit)
		if n, err := b.Write(nil); n != 0 || err != nil {
			t.Fatal("nonpositive limit did not clamp to zero")
		}
		if _, err := b.Write([]byte("x")); err == nil {
			t.Fatal("nonpositive buffer was unbounded")
		}
	}
}

func TestOverflowRejectsEvenSmallerAndEmptyWrites(t *testing.T) {
	b := New(4)
	_, _ = b.Write([]byte("ab"))
	_, _ = b.Write([]byte("123"))
	for _, input := range [][]byte{nil, []byte("c")} {
		if n, err := b.Write(input); n != 0 || err == nil || b.Len() != 2 {
			t.Fatal("overflow was not sticky")
		}
	}
}

func TestLimitDiagnosticsFitAtIntegerExtremes(t *testing.T) {
	for _, limit := range []int64{0, 1, math.MinInt64, math.MaxInt64} {
		e := &LimitError{Limit: limit}
		for text, max := range map[string]int{e.Reason(): 80, e.Remedy(): 120} {
			if err := publicerr.CheckMessage(text, max); err != nil {
				t.Fatal(err, text)
			}
		}
	}
}
