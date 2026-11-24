package gitcache_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/gitcache/gittest"
	"k8s.io/utils/clock"
)

func TestFirstPublicationSkipsReadBeforeFencedPush(t *testing.T) {
	p := newPublishFixture(t)
	// #1830 still removes redundant target preflights on the default branch.
	// Topic publication has a distinct required live-head/receive-head rule.
	p.intent.Ref = "refs/heads/main"
	p.predecessor(p.f.Commit)
	p.f.Git(t, p.f.Store(), "update-ref", p.intent.Ref, p.next)
	p.intent.Attempts = 1
	commands := filepath.Join(p.f.Root, "commands.jsonl")
	if err := os.WriteFile(commands, nil, 0600); err != nil {
		t.Fatal(err)
	}
	start := (clock.RealClock{}).Now()
	if _, err := p.cache.Publish(t.Context(), p.intent); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(commands)
	if err != nil {
		t.Fatal(err)
	}
	reads := strings.Count(string(data), `"ls-remote"`)
	t.Logf("first publication: %v; forge reads=%d pushes=%d", (clock.RealClock{}).Since(start), reads, publicationPushes(t, p.f))
	if reads != 1 || publicationPushes(t, p.f) != 1 {
		t.Fatal("first default attempt needs only its fenced push and read-back", string(data))
	}
	checkTargetReadFollowsPush(t, data, p.intent.Ref)
	p.intent.Attempts = 2
	if err := os.WriteFile(commands, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := p.cache.Publish(t.Context(), p.intent); err != nil {
		t.Fatal(err)
	}
	if publicationPushes(t, p.f) != 0 {
		t.Fatal("confirmed retry pushed again")
	}
}

func checkTargetReadFollowsPush(t *testing.T, data []byte, ref string) {
	t.Helper()
	pushed := false
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var command struct {
			Argv []string `json:"argv"`
		}
		if err := json.Unmarshal([]byte(line), &command); err != nil {
			t.Fatal(err)
		}
		if slices.Contains(command.Argv, "push") {
			pushed = true
		}
		if slices.Contains(command.Argv, "ls-remote") && slices.Contains(command.Argv, ref) && !pushed {
			t.Fatal("first publication read its target before the fenced push", line)
		}
	}
}

func TestFirstPublicationPreservesExistingTag(t *testing.T) {
	p := newPublishFixture(t)
	p.releaseTag()
	p.intent.Attempts = 1
	p.upstream(t, p.oldTag)
	if _, err := p.cache.Publish(t.Context(), p.intent); publicationClass(err) != "already-exists" {
		t.Fatal("tag collision lost its class", err)
	}
	if actual := p.f.Git(t, p.f.Bare, "rev-parse", p.intent.Ref); actual != p.oldTag {
		t.Fatal("first attempt overwrote a tag", actual)
	}
}

func TestInterruptedFirstPromotionReadsBackWithoutPreparedResult(t *testing.T) {
	f := gittest.New(t)
	c, prepared := preparedPromotion(t, f)
	intent := promotionPublication(f, prepared)
	intent.Attempts = 1
	if _, err := c.Publish(t.Context(), intent); err != nil {
		t.Fatal(err)
	}
	// The successful native publication released its prepared promotion, but
	// the process stopped before its durable attempt could be settled.
	intent.ReadBack = true
	if _, err := c.Publish(t.Context(), intent); err != nil {
		t.Fatal("interrupted first attempt did not read back", err)
	}
}
