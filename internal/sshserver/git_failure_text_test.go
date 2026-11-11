package sshserver

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache"
)

func TestGitFailureTextNamesTheRemedy(t *testing.T) {
	cause := errors.New("create run: database is locked")
	own := &gitcache.ReceiveDeliveryError{Repo: "github/acme/app", EventID: "r1:0", Ref: "refs/heads/feature", Err: cause}
	text, internal := gitFailureText(errors.Join(own))
	for _, want := range []string{"refs accepted", "refs/heads/feature", "database is locked", "(record r1)", "oberth outbox show github/acme/app", "oberth outbox retry github/acme/app"} {
		if !strings.Contains(text, want) {
			t.Fatalf("own failure omits %q:\n%s", want, text)
		}
	}
	if !internal {
		t.Fatal("the full chain must go to the server log")
	}

	earlier := &gitcache.ReceiveDeliveryError{Repo: "github/acme/app", EventID: "r1:0", Ref: "refs/heads/feature", Err: cause, Earlier: true}
	blocked := &gitcache.ReceiveBlockedError{Repo: "github/acme/app", Err: earlier}
	text, _ = gitFailureText(blocked)
	for _, want := range []string{"push refused", "earlier accepted push", "record r1)", "nothing from this push was applied", "oberth outbox show github/acme/app"} {
		if !strings.Contains(text, want) {
			t.Fatalf("blocked push omits %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "replay earlier receive") {
		t.Fatalf("raw chain reached the pusher:\n%s", text)
	}

	corrupt := &gitcache.ReceiveBlockedError{Repo: "github/acme/app", Err: fmt.Errorf("%w: repository github/acme/app contains invalid JSON", gitcache.ErrCorruptReceiveReservation)}
	text, _ = gitFailureText(corrupt)
	if !strings.Contains(text, "quarantined") || !strings.Contains(text, "oberth outbox show github/acme/app") {
		t.Fatalf("corrupt journal text:\n%s", text)
	}

	plain, internal := gitFailureText(errors.New("repository not found"))
	if plain != "oberth: repository not found\n" || internal {
		t.Fatalf("ordinary failure changed: %q", plain)
	}

	escaped := &gitcache.ReceiveDeliveryError{Repo: "r", EventID: "e", Ref: "x", Err: errors.New("bad\x1b[2J value\r\nnext")}
	if text, _ := gitFailureText(escaped); strings.ContainsAny(text, "\x1b\r") || !strings.Contains(text, "bad[2J value next") {
		t.Fatalf("control characters reached the pusher: %q", text)
	}

	long := &gitcache.ReceiveDeliveryError{Repo: "r", EventID: "e", Ref: "x", Err: errors.New(strings.Repeat("Q", 500))}
	if text, _ := gitFailureText(long); strings.Count(text, "Q") > 200 {
		t.Fatal("cause is unbounded")
	}

	// A blocked push whose cause is not a delivery error (a lock, an I/O or
	// outbox read failure) gets the same bounded line and the log.
	stuck := &gitcache.ReceiveBlockedError{Repo: "github/acme/app", Err: errors.New("read outbox: " + strings.Repeat("Z", 500) + "\x1b[2J")}
	text, internal = gitFailureText(stuck)
	if !internal || !strings.Contains(text, "push refused") || !strings.Contains(text, "oberth outbox show github/acme/app") || strings.Count(text, "Z") > 200 || strings.Contains(text, "\x1b") || strings.Contains(text, "replay earlier receive") {
		t.Fatalf("blocked push without a delivery error:\n%s", text)
	}
	// Any other long failure is bounded too; the chain goes to the log.
	text, internal = gitFailureText(errors.New(strings.Repeat("W", 500)))
	if !internal || strings.Count(text, "W") > 200 || !strings.HasSuffix(text, "\n") {
		t.Fatalf("long ordinary failure: %q", text)
	}
}

func TestReceiveRecordIDDropsTheUpdateIndex(t *testing.T) {
	for input, want := range map[string]string{"r1:0": "r1", "abc:12": "abc", "plain": "plain", "a:b": "a:b", ":3": ":3"} {
		if got := receiveRecordID(input); got != want {
			t.Errorf("receiveRecordID(%q) = %q, want %q", input, got, want)
		}
	}
}
