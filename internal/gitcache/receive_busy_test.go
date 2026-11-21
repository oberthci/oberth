package gitcache_test

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/gitcache/gittest"
)

func TestPendingDeliveryDoesNotClaimUnreadPushWasSaved(t *testing.T) {
	f := gittest.New(t)
	cfg := reviewConfig(t, f)
	cfg.PreReceive = func(context.Context, gitcache.ReceiveEvent, *gitcache.ReceiveSource) error { return nil }
	c := thirdInit(t, cfg)
	record, _ := pendingAbandonReceive(t, c, f)
	path := filepath.Join(f.Store(), "oberth-receive.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	input := &unreadPush{t: t}
	r := gateReceive(f)
	r.Input = input
	if err := c.ReplayPending(t.Context(), func(_ context.Context, e gitcache.ReceiveEvent) ([]string, error) {
		if e.ID != record.Event.ID {
			t.Error("another receive was replayed")
		}
		return nil, io.ErrClosedPipe
	}); err == nil {
		t.Fatal("broken delivery was not set aside")
	}
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) {
		t.Error("new push retried a set-aside delivery")
		return nil, nil
	}
	err = c.Receive(t.Context(), r)
	if err == nil || !strings.Contains(err.Error(), "previous push needs attention") || !strings.Contains(err.Error(), "oberth receive outbox") || strings.Contains(err.Error(), "push saved") {
		t.Fatal("misleading second-push refusal", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("unread push changed the reservation", err)
	}
	if refs := f.Git(t, f.Store(), "for-each-ref", "--format=%(refname)", "refs/heads"); strings.Contains(refs, "second") {
		t.Fatal("unread push moved refs", refs)
	}
}

type unreadPush struct{ t *testing.T }

func (r *unreadPush) Read([]byte) (int, error) {
	r.t.Error("refused push input was read")
	return 0, io.EOF
}
func (*unreadPush) Close() error { return nil }
