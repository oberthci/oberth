package boundedio

import (
	"errors"
	"testing"
)

func TestBufferRefusesOverflowAndCancelsOnce(t *testing.T) {
	overflow := errors.New("too large")
	calls := 0
	b := &Buffer{Limit: 4, LimitError: overflow, OnOverflow: func() { calls++ }}
	if n, err := b.Write([]byte("1234")); n != 4 || err != nil {
		t.Fatalf("exact limit: %d %v", n, err)
	}
	for range 2 {
		if n, err := b.Write([]byte("5")); n != 0 || !errors.Is(err, overflow) {
			t.Fatalf("overflow: %d %v", n, err)
		}
	}
	if calls != 1 || b.String() != "1234" || !errors.Is(b.Err(), overflow) {
		t.Fatalf("calls=%d body=%q err=%v", calls, b.String(), b.Err())
	}
}
