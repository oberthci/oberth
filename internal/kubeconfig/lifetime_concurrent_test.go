package kubeconfig

import (
	"os"
	"sync"
	"testing"
)

func TestConcurrentPrivateCopyLifetimes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	var group sync.WaitGroup
	failures := make(chan error, 400)
	for range 4 {
		group.Go(func() {
			for range 100 {
				_, _, cleanup, err := newPrivateCopy()
				if err == nil {
					err = cleanup()
				}
				if err != nil {
					failures <- err
				}
			}
		})
	}
	group.Wait()
	close(failures)
	count := 0
	for err := range failures {
		if count < 5 {
			t.Log(err)
		}
		count++
	}
	if count != 0 {
		t.Fatalf("%d of 400 ordinary concurrent copy lifetimes failed", count)
	}
	if err := Sweep(); err != nil {
		t.Fatal(err)
	}
	root, err := copyRootPath()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("completed private copies remain: %d entries, %v", len(entries), err)
	}
}
