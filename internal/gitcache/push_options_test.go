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
	packet("oberth.assist=1")
	packet("oberth.priority_reason=urgent repair")
	packet("oberth.priorty=high")
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
	if live.ActorFingerprint != "SHA256:original-key" || live.Priority != "highest" || !live.Supersede || !live.Assist || live.Actor != "original-requester" || live.PriorityReason != "urgent repair" {
		t.Fatalf("lost push options: %+v", live)
	}
	if !strings.Contains(output.String(), "ignored unknown push option oberth.priorty") {
		t.Fatalf("unknown push option was ignored silently:\n%s", output.String())
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
		parsed := parsePushOptions(options)
		if parsed.priority != "invalid" || parsed.reason == "" {
			t.Fatalf("invalid request lost: %q %q", parsed.priority, parsed.reason)
		}
	}
	parsed := parsePushOptions([]string{"oberth.priority=high", "oberth.supersede=false", "oberth.assist=0"})
	if parsed.assist {
		t.Fatal("oberth.assist=0 asked for assistance")
	}
	if len(parsed.notices) != 0 {
		t.Fatalf("explicit false values are known options: %q", parsed.notices)
	}
	if !parsePushOptions([]string{"oberth.assist=1"}).assist {
		t.Fatal("oberth.assist=1 ignored")
	}
	if !parsePushOptions([]string{"oberth.assist=true"}).assist {
		t.Fatal("oberth.assist=true ignored")
	}
	if parsed.priority != "high" || parsed.supersede {
		t.Fatal("false supersede enabled cancellation")
	}
}

// docs/release-priority.md documents oberth.priority_reason; it reaches the
// run as the request's reason.
func TestPriorityReasonPushOptionIsRead(t *testing.T) {
	parsed := parsePushOptions([]string{"oberth.priority=high", "oberth.priority_reason=urgent repair"})
	if parsed.priority != "high" || parsed.reason != "urgent repair" || len(parsed.notices) != 0 {
		t.Fatalf("parsed = %+v", parsed)
	}
	for _, bad := range []string{"oberth.priority_reason=", "oberth.priority_reason=" + strings.Repeat("x", 513), "oberth.priority_reason=a\x01b"} {
		parsed := parsePushOptions([]string{"oberth.priority=high", bad})
		if parsed.reason != "git push priority request" || len(parsed.notices) != 1 {
			t.Fatalf("%q: parsed = %+v", bad, parsed)
		}
	}
	if parsed := parsePushOptions([]string{"oberth.priority_reason=why"}); parsed.priority != "" || len(parsed.notices) != 1 {
		t.Fatalf("a reason without a priority must be noticed: %+v", parsed)
	}
}

func TestUnknownOberthPushOptionsAreNoticed(t *testing.T) {
	parsed := parsePushOptions([]string{"oberth.priorty=high", "oberth.supersede=yes", "ci.skip", "oberth.\x1bbad"})
	if len(parsed.notices) != 3 || parsed.priority != "" || parsed.supersede {
		t.Fatalf("parsed = %+v", parsed)
	}
	if !strings.Contains(parsed.notices[0], "oberth.priorty") || !strings.Contains(parsed.notices[0], "oberth.priority_reason") {
		t.Fatalf("notice does not name the option and the known ones: %q", parsed.notices[0])
	}
	if strings.ContainsRune(parsed.notices[2], '\x1b') {
		t.Fatalf("control characters reached the pusher: %q", parsed.notices[2])
	}
}
