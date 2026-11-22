package gitcache

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// Every admission refused at its deadline is logged once with its
// repository, whether its callback was still running (the gate's select saw
// the deadline first) or had just returned (the select took its answer):
// both branches are taken over 64 rounds.
func TestEveryDeadlineRefusalIsLoggedWithItsRepository(t *testing.T) {
	var logs []string
	c := &Cache{config: Config{Log: func(reason, detail string) { logs = append(logs, reason+": "+detail) }}}
	source := &ReceiveSource{event: ReceiveEvent{ID: "receive", Repository: "origin/team/widget"}}
	expired, cancel := context.WithDeadline(t.Context(), time.Unix(0, 0))
	defer cancel()
	for round := range 64 {
		done := make(chan error, 1)
		done <- errors.New("the callback gave up at its own deadline")
		answered, err := c.admissionAnswer(expired, "receive", source, done)
		reply := c.admissionVerdict(expired, source, answered, err)
		if reply == nil || !strings.Contains(reply.Error(), reasonGateTimeout) {
			t.Fatal("the admission was not refused as out of time", reply)
		}
		if len(logs) != round+1 || !strings.Contains(logs[round], ": origin/team/widget: receive") {
			t.Fatal("a deadline refusal was not logged once with its repository", answered, logs)
		}
	}
}
