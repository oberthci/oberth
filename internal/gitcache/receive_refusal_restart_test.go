package gitcache_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestUnwrittenRefusalIsLostAcrossRestart(t *testing.T) {
	f, c, _ := refuseWithBrokenStorage(t)
	record := readCompletionRecord(t, f)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(f.Store(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.Store(), "refs", "heads", "topic.lock")); err != nil {
		t.Fatal(err)
	}
	// Reopening has only durable evidence; no operation first saves the refusal.
	c = reviewNew(t, reviewConfig(t, f))
	var events []gitcache.ReceiveEvent
	retryCompletion(t, c, func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		events = append(events, event)
		return nil, nil
	})
	assertPreparedTransaction(t, f, events)
	if events[0].ID != record.Event.ID {
		t.Fatal("restart changed durable receive identity", events)
	}
}
