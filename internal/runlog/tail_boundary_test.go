package runlog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTailPreservesRecordAndRuneBoundaries(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f, err := s.Create("tail-boundary")
	if err != nil {
		t.Fatal(err)
	}
	last := "[test/unit] 完了\n"
	body := "[test/unit] " + strings.Repeat("界", 30) + "\n" + last
	if _, err = f.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	for _, maximum := range []int64{int64(len(last) + 2), int64(len(last)), int64(len(body))} {
		got, err := s.Tail("tail-boundary", maximum)
		if err != nil || !utf8.Valid(got) || !strings.HasPrefix(string(got), "[test/unit] ") || int64(len(got)) > maximum || !strings.HasSuffix(string(got), last) {
			t.Fatalf("Tail(%d) = %q, %v", maximum, got, err)
		}
	}
}
