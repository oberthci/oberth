package runner

import (
	"bytes"
	"testing"
)

func TestPrefixedWriterEmitsCarriageReturnProgressBeforeFlush(t *testing.T) {
	var output bytes.Buffer
	writer := newPrefixedWriter(&output, "[setup/fetch] ", NewMasker([]string{"download-token"}))

	if _, err := writer.Write([]byte("download 10% download-token\r")); err != nil {
		t.Fatal(err)
	}
	const want = "[setup/fetch] download 10% ***\n"
	if got := output.String(); got != want {
		t.Fatalf("output before Flush = %q, want %q", got, want)
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	if got := output.String(); got != want {
		t.Fatalf("output after Flush = %q, want no duplicate progress frame %q", got, want)
	}
}

func TestPrefixedWriterTreatsSplitCRLFAsOneDelimiter(t *testing.T) {
	var output bytes.Buffer
	writer := newPrefixedWriter(&output, "[setup/fetch] ", Masker{})

	for _, fragment := range []string{"first\r", "\nsecond\r\nthird\n"} {
		if _, err := writer.Write([]byte(fragment)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Flush(); err != nil {
		t.Fatal(err)
	}
	const want = "[setup/fetch] first\n[setup/fetch] second\n[setup/fetch] third\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestPrefixedWriterMasksBareCarriageReturnSecretLines(t *testing.T) {
	var output bytes.Buffer
	writer := newPrefixedWriter(&output, "[setup/fetch] ", NewMasker([]string{"first-secret\rsecond-secret"}))

	if _, err := writer.Write([]byte("first-secret\rsecond-secret\r")); err != nil {
		t.Fatal(err)
	}
	const want = "[setup/fetch] ***\n[setup/fetch] ***\n"
	if got := output.String(); got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}
