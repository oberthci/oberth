package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// Input that is not a terminal reads the review whole, as for piped answers.
func TestReviewTerminalPagesOnlyATerminal(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	for _, in := range []io.Reader{strings.NewReader("y\n"), read} {
		if terminal := reviewTerminal(in, nil, io.Discard); terminal.Rows != 0 || terminal.Columns != 0 {
			t.Fatal("non-terminal input is paged", terminal.Rows, terminal.Columns)
		}
	}
}
