package runlog

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestLogIndexRejectsLinksAndNonregular(t *testing.T) {
	for _, kind := range []string{"link", "directory", "fifo"} {
		t.Run(kind, func(t *testing.T) {
			s := newStore(t, 64)
			name := filepath.Join(s.directory, "run.index.json")
			var err error
			switch kind {
			case "link":
				if err := os.WriteFile(filepath.Join(s.directory, "source"), []byte(`{"run_id":"run","size":0,"containers":[]}`), 0600); err != nil {
					t.Fatal(err)
				}
				err = os.Symlink("source", name)
			case "directory":
				err = os.Mkdir(name, 0700)
			case "fifo":
				err = syscall.Mkfifo(name, 0600)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.Containers(t.Context(), "run"); err == nil {
				t.Fatal("nonregular index accepted")
			}
			usage, err := s.Usage(t.Context(), "run", usageClock())
			if err != nil || usage.AccountingComplete || !usage.Incomplete {
				t.Fatal("unreadable index hidden by usage", usage, err)
			}
		})
	}
}

func TestMissingLogIndexAndCanceledUsage(t *testing.T) {
	s := newStore(t, 64)
	entries, err := s.Containers(t.Context(), "run")
	if err != nil || entries == nil || len(entries) != 0 {
		t.Fatal("missing index changed empty result", entries, err)
	}
	usage, err := s.Usage(t.Context(), "run", usageClock())
	if err != nil || !usage.AccountingComplete || usage.Incomplete || usage.Body.Logical != 0 || usage.Index.Logical != 0 {
		t.Fatal("missing index is not empty", usage, err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Usage(ctx, "run", usageClock()); !errors.Is(err, context.Canceled) {
		t.Fatal("usage ignored cancellation", err)
	}
}
