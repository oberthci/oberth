package argojob

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"testing"
)

// TestCopyPrefixedStopsOnBudgetExceeded proves that newline-dense input hits
// the per-step byte budget and stops reading promptly rather than buffering
// the entire stream in memory.
func TestCopyPrefixedStopsOnBudgetExceeded(t *testing.T) {
	t.Parallel()
	// Generate input that will exceed the budget: many short lines produce
	// many prefixed writes. Each line is ~5 bytes of content plus the prefix.
	step := StepResult{Burn: "test", Step: "log"}
	prefix := "[test/log] "
	lineContent := "x\n"
	// Each output line is len(prefix) + 1 + 1 = ~13 bytes. We need enough
	// lines to exceed maxStepLogBytes.
	lineCount := (maxStepLogBytes / int64(len(prefix)+len(lineContent))) + 1000
	input := strings.Repeat(lineContent, int(lineCount))

	var destination bytes.Buffer
	err := copyPrefixed(&destination, step, strings.NewReader(input))
	if err == nil {
		t.Fatal("expected an error when the budget is exceeded")
	}
	if !strings.Contains(err.Error(), "budget") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The destination should have received some output but not the full input.
	if destination.Len() == 0 {
		t.Fatal("destination received no output before the budget was hit")
	}
	if int64(destination.Len()) > maxStepLogBytes+4096 {
		t.Fatalf("destination received %d bytes, expected roughly %d",
			destination.Len(), maxStepLogBytes)
	}
}

type failWriter struct {
	limit   int
	written int
}

func (w *failWriter) Write(p []byte) (int, error) {
	w.written += len(p)
	if w.written > w.limit {
		return 0, errors.New("destination full")
	}
	return len(p), nil
}

// TestCopyPrefixedStopsOnDestinationFailure proves that a destination write
// error stops the copy immediately rather than reading the entire stream into
// an unbounded buffer.
func TestCopyPrefixedStopsOnDestinationFailure(t *testing.T) {
	t.Parallel()
	step := StepResult{Burn: "build", Step: "compile"}
	// Many lines of input; destination fails after 100 bytes.
	input := strings.Repeat("some output line\n", 100000)
	destination := &failWriter{limit: 100}

	err := copyPrefixed(destination, step, strings.NewReader(input))
	if err == nil {
		t.Fatal("expected an error when the destination fails")
	}
	if !strings.Contains(err.Error(), "destination full") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestCopyPrefixedPrefixesEveryLine proves that every line of output is
// correctly prefixed with the burn/step marker.
func TestCopyPrefixedPrefixesEveryLine(t *testing.T) {
	t.Parallel()
	step := StepResult{Burn: "setup", Step: "prepare"}
	input := "line one\nline two\nline three"
	var destination bytes.Buffer
	if err := copyPrefixed(&destination, step, strings.NewReader(input)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	lines := strings.Split(strings.TrimSuffix(destination.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3: %q", len(lines), destination.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "[setup/prepare] ") {
			t.Errorf("line not prefixed: %q", line)
		}
	}
}

// TestCopyPrefixedAssemblesOneWrite proves each prefixed line is emitted as a
// single Write call rather than three separate writes (prefix, content,
// newline), which reduces syscall overhead and avoids interleaving.
func TestCopyPrefixedAssemblesOneWrite(t *testing.T) {
	t.Parallel()
	step := StepResult{Burn: "a", Step: "b"}
	input := "hello\nworld\n"

	var writes []string
	writer := writeCounter{record: &writes}
	if err := copyPrefixed(writer, step, strings.NewReader(input)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Two content lines means two Write calls.
	if len(writes) != 2 {
		t.Fatalf("got %d Write calls, want 2: %v", len(writes), writes)
	}
	for _, w := range writes {
		if !strings.HasPrefix(w, "[a/b] ") || !strings.HasSuffix(w, "\n") {
			t.Errorf("write %q is not a single assembled line", w)
		}
	}
}

type writeCounter struct {
	record *[]string
}

func (w writeCounter) Write(p []byte) (int, error) {
	*w.record = append(*w.record, string(p))
	return len(p), nil
}

// TestCopyPrefixedHandlesEmptyInput proves that an empty body produces no
// output and no error.
func TestCopyPrefixedHandlesEmptyInput(t *testing.T) {
	t.Parallel()
	step := StepResult{Burn: "a", Step: "b"}
	var destination bytes.Buffer
	if err := copyPrefixed(&destination, step, io.LimitReader(strings.NewReader(""), 0)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if destination.Len() != 0 {
		t.Fatalf("expected no output, got %q", destination.String())
	}
}
