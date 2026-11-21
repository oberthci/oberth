package gitcache

import (
	"sync"
	"testing"
)

func TestReceiveCleanupForgetsOnlyItsOwnEntry(t *testing.T) {
	c := &Cache{activeMu: &sync.Mutex{}}
	store := t.TempDir()
	older := c.trackReceiveFiles(store, "older")
	newer := c.trackReceiveFiles(store, "newer")
	c.forgetReceiveFiles(store, older)
	if c.receiveCleanupFor(store) != newer {
		t.Fatal("older cleanup dropped the newer receive's entry")
	}
	c.forgetReceiveFiles(store, newer)
	if c.receiveCleanupFor(store) != nil {
		t.Fatal("cleanup retained its own finished entry")
	}
}
