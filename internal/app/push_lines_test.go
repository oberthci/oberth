package app_test

import (
	"context"
	"strings"
	"testing"

	"github.com/oberthci/oberth/internal/app"
)

func TestPushFeedbackIsCompleteWithoutInternalCodes(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		out  app.Outcome
		want string
	}{
		{"parked", app.Outcome{Ref: "refs/heads/topic", State: "parked"}, "refs/heads/topic: branch parked; nothing ran. Run it with git push -o oberth.run=1."},
		{"deleted", app.Outcome{Ref: "refs/heads/topic", State: "deleted"}, "refs/heads/topic: ref deleted; nothing ran."},
		{"absent", app.Outcome{Ref: "refs/heads/topic", File: ".oberth/build.yaml", State: "absent"}, "refs/heads/topic .oberth/build.yaml: no pipeline; nothing ran. Add the selected pipeline file to start CI."},
		{"accepted", app.Outcome{Ref: "refs/heads/topic", RunID: "r1", State: "accepted", WatchURL: "https://oberth.ci/runs/r1"}, "refs/heads/topic r1: run accepted.\n  watch: https://oberth.ci/runs/r1"},
		{"started", app.Outcome{Ref: "refs/heads/topic", RunID: "r1", State: "started"}, "refs/heads/topic r1: run started."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := (app.PushResult{Outcomes: []app.Outcome{tc.out}}).Lines()
			if len(lines) != 1 || lines[0] != tc.want {
				t.Fatal(lines)
			}
		})
	}
}
func TestGateTimeoutHasOnePushRefusalHeader(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	s := f.new(t)
	r := receive()
	r.Source = f.source
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.Gate(ctx, r)
	if err == nil {
		t.Fatal("expired gate accepted")
	}
	text := err.Error()
	if !strings.HasPrefix(text, "push refused; nothing ran\nadmission timed out. Try the push again.") {
		t.Fatal(text)
	}
	if strings.Contains(text, "timeout:") || strings.Contains(text, "  :") || strings.Contains(text, "context canceled") || strings.Contains(text, "detail:") {
		t.Fatal("internal code or empty field", text)
	}
}
