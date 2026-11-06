package sshserver

import (
	"context"
	"reflect"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

type queuedWatchHandler struct{ ids []string }

func (h queuedWatchHandler) HandleReceive(context.Context, gitcache.ReceiveEvent) error { return nil }
func (h queuedWatchHandler) QueuedRunIDs() []string                                     { return h.ids }
func TestPushWatchLinksDurableRunsOrBareDashboard(t *testing.T) {
	for _, base := range []string{"https://watch.example", "https://watch.example/", "https://watch.example/runs"} {
		if got := pushWatchURLs(base, queuedWatchHandler{[]string{"abc123", "abc123", "def456"}}); !reflect.DeepEqual(got, []string{"https://watch.example/runs/abc123", "https://watch.example/runs/def456"}) {
			t.Fatal(got)
		}
		if got := pushWatchURLs(base, queuedWatchHandler{}); !reflect.DeepEqual(got, []string{base}) {
			t.Fatal(got)
		}
	}
}
