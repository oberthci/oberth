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

func TestAssistPushOptionsSurviveReceiveAndReplay(t *testing.T) {
	repository := newTestRepository(t)
	cache := newTestCache(t, repository.upstream)
	if _, err := cache.Ensure(t.Context(), "example"); err != nil {
		t.Fatal(err)
	}
	var input bytes.Buffer
	packet := func(s string) { _, _ = fmt.Fprintf(&input, "%04x%s", len(s)+4, s) }
	packet(strings.Repeat("0", 40) + " " + repository.initialSHA + " refs/heads/urgent\x00report-status push-options\n")
	input.WriteString("0000")
	packet("oberth.assist=1")
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
	if live.ActorFingerprint != "SHA256:original-key" || !live.Assist || live.Actor != "original-requester" {
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

func TestRetiredSchedulingPushOptionsRefuseReceive(t *testing.T) {
	for _, option := range []string{"oberth.priority=high", "oberth.priority_reason=repair"} {
		got := parsePushOptions([]string{option})
		if got.err == nil || !strings.Contains(got.err.Error(), "not supported") {
			t.Fatalf("%s: %+v", option, got)
		}
	}
	for _, option := range []string{"oberth.assist=1", "oberth.assist=true"} {
		if got := parsePushOptions([]string{option}); !got.assist || got.err != nil {
			t.Fatal(got)
		}
	}
	got := parsePushOptions([]string{"oberth.unknown=1"})
	if len(got.notices) != 1 || !strings.Contains(got.notices[0], "oberth.assist=1") {
		t.Fatal(got)
	}
}
