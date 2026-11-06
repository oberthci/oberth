package main

import (
	"io"
	"os"
	"testing"
)

func TestReportModeOnlyReportsAlternateModes(t *testing.T) {
	for _, mode := range []string{"server", "local"} {
		t.Run(mode, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "stderr")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			original := os.Stderr
			os.Stderr = file
			defer func() { os.Stderr = original }()
			reportMode(mode)
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(file)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if mode != "server" {
				want = "reading: local\n"
			}
			if string(body) != want {
				t.Fatalf("got %q, want %q", body, want)
			}
		})
	}
}
