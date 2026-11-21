package argoworkflow

import (
	"context"
	"github.com/oberthci/oberth/pkg/argoworkflow/controllertest"
	"os"
	"os/exec"
	"testing"
	"time"
)

func controllerContext(t *testing.T) context.Context {
	t.Helper()
	deadline, ok := t.Deadline()
	if !ok {
		t.Fatal("controller harness requires an outer test deadline")
	}
	if value := os.Getenv("OBERTH_CONTROLLER_DEADLINE"); value != "" {
		inherited, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			t.Fatal(err)
		}
		if inherited.Before(deadline) {
			deadline = inherited
		}
	}
	ctx, cancel := context.WithDeadline(t.Context(), deadline.Add(-5*time.Second))
	t.Cleanup(cancel)
	return ctx
}

func controllerOutput(command *exec.Cmd) ([]byte, error) {
	return controllertest.Output(command)
}

func TestControllerCommandPrecedesOuterAndInheritedDeadlines(t *testing.T) {
	outer, ok := t.Deadline()
	if !ok {
		t.Fatal("test deadline missing")
	}
	for _, tc := range []struct{ inherited, expected time.Time }{
		{outer.Add(time.Minute), outer.Add(-5 * time.Second)},
		{outer.Add(-time.Minute), outer.Add(-65 * time.Second)},
	} {
		t.Setenv("OBERTH_CONTROLLER_DEADLINE", tc.inherited.Format(time.RFC3339Nano))
		ctx := controllerContext(t)
		got, ok := ctx.Deadline()
		if !ok || !got.Equal(tc.expected) {
			t.Fatal("command deadline does not precede every enclosing test")
		}
	}
}
