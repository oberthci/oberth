package gitcache_test

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
	"github.com/oberthci/oberth/internal/sourcecontract"
)

// A pipeline binds a pin once per declaration (each template and artifact).
// With the producer's forge not answering its head read, the declarations of
// one admission wait for that read once, not once each: twelve declarations
// of the same producer fit the real 30-second admission with the real
// 3-second head wait, and the recently refreshed cache decides them.
func TestPinDeclarationsShareOneWaitForAnUnansweredHeadRead(t *testing.T) {
	x := newPinFixture(t)
	var failures []string
	x.cfg.PreReceive = func(ctx context.Context, _ gitcache.ReceiveEvent, s *gitcache.ReceiveSource) error {
		for i := range 12 {
			if _, reachable, err := s.Pin(ctx, sourcecontract.Revision{Repository: pinnedWire, Commit: x.wire.Commit}); err != nil || !reachable {
				failures = append(failures, fmt.Sprint(i, reachable, err))
			}
		}
		if len(failures) > 0 {
			return fmt.Errorf("pins refused: %s", strings.Join(failures, "; "))
		}
		return nil
	}
	x.start(t)
	x.forge.arm(t, "wire")
	var diagnostics strings.Builder
	r := gateReceive(x.f)
	r.AlwaysRun = true
	r.Input = receiveRefInput(t, x.f, gitcache.RefUpdate{Ref: "refs/heads/land/oberth-v030", NewSHA: x.f.Commit})
	r.Output, r.Diagnostics = io.Discard, &diagnostics
	r.Deliver = func(context.Context, gitcache.ReceiveEvent) ([]string, error) { return nil, nil }
	if err := x.c.Receive(t.Context(), r); err != nil {
		t.Fatalf("a push with many declarations of one silent producer was refused: %v\n%s", err, diagnostics.String())
	}
}
