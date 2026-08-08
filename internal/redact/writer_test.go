package redact

import (
	"bytes"
	"strings"
	"testing"
)

func TestWriterMasksAcrossWriteBoundaries(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("correct-horse-battery-staple")})
	for _, fragment := range []string{"before correct-", "horse-battery", "-staple after"} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "before *** after" {
		t.Fatalf("output = %q", got)
	}
}

func TestWriterMasksLongestValueFirstAndEveryNonemptyValue(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{
		[]byte("token"), []byte("token-long"), []byte("abc"), []byte("first-line\r\nsecond-line\n"), nil,
	})
	_, _ = writer.Write([]byte("token-long token abc first-line second-line first-line\r\nsecond-line"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != "*** *** *** *** *** ***" {
		t.Fatalf("output = %q", got)
	}
}

func TestClosedWriterRejectsWritesWithoutLeakingInput(t *testing.T) {
	var output bytes.Buffer
	writer := NewWriter(&output, [][]byte{[]byte("secret")})
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("secret")); err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("error = %v", err)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q", output.String())
	}
}
