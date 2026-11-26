package client

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"golang.org/x/sys/unix"
)

func TestMountedPrivateRejectsVersionSwapOutsideMount(t *testing.T) {
	path := kubeletMount(t, "..2026_11_25_19_40_09.1")
	mount := filepath.Dir(path)
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "ca.crt"), []byte("outside-fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	version, alternate := filepath.Join(mount, "..2026_11_25_19_40_09.1"), filepath.Join(mount, "..alternate")
	if err := os.Symlink(outside, alternate); err != nil {
		t.Fatal(err)
	}
	var stop atomic.Bool
	done := make(chan error, 1)
	go func() {
		for !stop.Load() {
			if err := unix.Renameat2(unix.AT_FDCWD, version, unix.AT_FDCWD, alternate, unix.RENAME_EXCHANGE); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	defer func() {
		stop.Store(true)
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	for range 20000 {
		data, err := ReadMountedPrivate(path)
		escaped := err == nil && string(data) == "outside-fixture"
		clear(data)
		if escaped {
			t.Fatal("mounted private read escaped through a swapped version directory")
		}
	}
}
