package gitcache

import (
	"bytes"
	"context"
	"crypto/sha1" // #nosec G505 -- Git's object protocol uses SHA-1.
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestPriorityPushOptionsSurviveReceiveAndReplay(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	packet := func(s string) { _, _ = fmt.Fprintf(&input, "%04x%s", len(s)+4, s) }
	packet(strings.Repeat("0", 40) + " " + repository.initialSHA + " refs/heads/urgent\x00report-status push-options\n")
	input.WriteString("0000")
	packet("oberth.priority=highest")
	packet("oberth.supersede=true")
	input.WriteString("0000")
	emptyPack := []byte{'P', 'A', 'C', 'K', 0, 0, 0, 2, 0, 0, 0, 0}
	checksum := sha1.Sum(emptyPack)
	input.Write(emptyPack)
	input.Write(checksum[:])
	var output bytes.Buffer
	deliveryFailure := errors.New("delivery interrupted")
	var live ReceiveEvent
	err := cache.Receive(WithReceiveFingerprint(t.Context(), "SHA256:original-key"), "example", "original-requester", "", &input, &output, &output, ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error { live = event; return deliveryFailure }))
	if !errors.Is(err, deliveryFailure) {
		t.Fatalf("receive: %v\n%s", err, output.String())
	}
	if live.ActorFingerprint != "SHA256:original-key" || live.Priority != "highest" || !live.Supersede || live.Actor != "original-requester" || live.PriorityReason != "git push priority request" {
		t.Fatalf("lost push options: %+v", live)
	}
	restarted := newTestCacheAt(t, cache.root, repository.upstream)
	var replay ReceiveEvent
	if err := restarted.ReplayPending(t.Context(), ReceiveHandlerFunc(func(_ context.Context, event ReceiveEvent) error { replay = event; return nil })); err != nil {
		t.Fatal(err)
	}
	if replay != live {
		t.Fatalf("replay changed priority intent: live=%+v replay=%+v", live, replay)
	}
}

func TestPriorityPushOptionsInvalidRequestsDoNotRejectPush(t *testing.T) {
	for _, options := range [][]string{{"oberth.priority="}, {"oberth.priority=high", "oberth.priority=highest"}, {"oberth.priority=" + strings.Repeat("x", 1024)}} {
		p, reason, _ := parsePushOptions(options)
		if p != "invalid" || reason == "" {
			t.Fatalf("invalid request lost: %q %q", p, reason)
		}
	}
	p, _, supersede := parsePushOptions([]string{"oberth.priority=high", "oberth.supersede=false"})
	if p != "high" || supersede {
		t.Fatal("false supersede enabled cancellation")
	}
}
