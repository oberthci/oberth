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
	for _, want := range []string{"refs accepted", "refs/heads/feature", "database is locked", "record r1:0", "oberth outbox show github/acme/app", "oberth outbox retry github/acme/app"} {
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
	for _, want := range []string{"push refused", "earlier accepted push", "record r1:0", "nothing from this push was applied", "oberth outbox show github/acme/app"} {
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

	long := &gitcache.ReceiveDeliveryError{Repo: "r", EventID: "e", Ref: "x", Err: errors.New(strings.Repeat("Q", 500))}
	if text, _ := gitFailureText(long); strings.Count(text, "Q") > 200 {
		t.Fatal("cause is unbounded")
	}
}
