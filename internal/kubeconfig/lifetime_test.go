package kubeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSweepPreservesMalformedOwnershipMarkers(t *testing.T) {
	for _, mode := range []string{"unknown field", "trailing data", "oversized", "staging"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("XDG_CACHE_HOME", t.TempDir())
			directory, lease, _, err := newPrivateCopy()
			if err != nil {
				t.Fatal(err)
			}
			if err := lease.Close(); err != nil {
				t.Fatal(err)
			}
			marker := filepath.Join(directory, ".owner")
			body, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "unknown field":
				body = append([]byte(`{"Unrecognized":true,`), body[1:]...)
			case "trailing data":
				body = append(body, []byte(`{}`)...)
			case "oversized":
				body = append(body, []byte(strings.Repeat(" ", 4096))...)
			case "staging":
				if err := os.Rename(marker, filepath.Join(directory, ".owner.pending")); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "staging" {
				if err := os.WriteFile(marker, body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			err = Sweep()
			if (err == nil) != (mode == "staging") {
				t.Fatalf("unexpected marker result: %v", err)
			}
			if _, err := os.Stat(directory); err != nil {
				t.Fatal("unverified object was reclaimed", err)
			}
		})
	}
}

func TestSweepReclaimsOnlyUnlockedOwnedCopies(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	directory, lease, cleanup, err := newPrivateCopy()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cleanup() }()
	if err := os.WriteFile(filepath.Join(directory, "config"), []byte("dummy credential"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal("active copy reclaimed", err)
	}
	// Simulate a dead owner: the kernel no longer holds its lease.
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	root := filepath.Dir(directory)
	foreign := filepath.Join(root, "copy-unmarked")
	if err := os.Mkdir(foreign, 0700); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(root, "copy-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatal("orphan survived", err)
	}
	for _, p := range []string{foreign, link, target} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatal("foreign object changed", p, err)
		}
	}
}

func TestSweepPreservesUnverifiedAgeAndReportsCleanupFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	directory, lease, _, err := newPrivateCopy()
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, ".owner")
	data, _ := json.Marshal(copyOwner{Format: 1, Name: filepath.Base(directory), CreatedAt: time.Now().Add(time.Hour)})
	if err := os.WriteFile(marker, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Sweep(); err == nil {
		t.Fatal("future lifetime silently accepted")
	}
	if _, err := os.Stat(directory); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(directory); err != nil {
		t.Fatal(err)
	}
	directory, _, cleanup, err := newPrivateCopy()
	if err != nil {
		t.Fatal(err)
	}
	moved := directory + "-moved"
	if err := os.Rename(directory, moved); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	if err := os.Symlink(target, directory); err != nil {
		t.Fatal(err)
	}
	if err := cleanup(); err == nil {
		t.Fatal("replacement/cleanup failure was hidden")
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal("replacement target removed", err)
	}
}
