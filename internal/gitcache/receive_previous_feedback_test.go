package gitcache_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestNewPushDoesNotReceivePreviousRestorationNotices(t *testing.T) {
	f, c, _ := blockedDeliveryReceive(t)
	previous, err := c.PendingReceive(t.Context(), gittest.Repository)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(filepath.Join(f.Store(), "refs", "heads"), 0700); err != nil {
		t.Fatal(err)
	}
	r := fourthAgentPush(t, f, []gitcache.RefUpdate{{Ref: "refs/heads/current", NewSHA: f.Commit}}, nil)
	var status, feedback bytes.Buffer
	r.Output, r.Diagnostics = &status, &feedback
	var delivered []gitcache.ReceiveEvent
	r.Deliver = func(_ context.Context, event gitcache.ReceiveEvent) ([]string, error) {
		delivered = append(delivered, event)
		if event.ID == previous.Event.ID {
			return []string{"previous receipt"}, nil
		}
		return []string{"current receipt"}, nil
	}
	if err = c.Receive(t.Context(), r); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 || delivered[0].ID != previous.Event.ID {
		t.Fatal("receives were not delivered separately", delivered)
	}
	want := [][]gitcache.RefUpdate{{{Ref: "refs/heads/topic", OldSHA: f.Commit}}, {{Ref: "refs/heads/current", NewSHA: f.Commit}}}
	if !reflect.DeepEqual([][]gitcache.RefUpdate{delivered[0].Updates, delivered[1].Updates}, want) {
		t.Fatal("receives were not delivered separately", delivered)
	}
	if feedback.String() != "current receipt\n" || !strings.Contains(status.String(), "ok refs/heads/current") {
		t.Fatal("current pusher received another push's notices", feedback.String(), status.String())
	}
	if f.Git(t, f.Store(), "rev-parse", "refs/heads/second") != f.Commit || completionState(t, f) != "absent" {
		t.Fatal("previous restoration or current delivery was lost")
	}
}
