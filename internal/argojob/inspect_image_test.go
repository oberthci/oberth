package argojob_test

import (
	"context"
	"github.com/oberthci/oberth/internal/argojob"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBMissingOrReplacedHelperHasNoRawFallback(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"missing", "empty", "nonexecutable", "replaced"} {
		t.Run(kind, func(t *testing.T) { checkBrokenHelper(t, kind) })
	}
}
func checkBrokenHelper(t *testing.T, kind string) {
	t.Helper()
	c, _, pods, b := inspectFixture(t)
	e := argojob.New(c)
	holds, err := e.Holds(t.Context(), b)
	compositionOK(t, err)
	binary := filepath.Join(t.TempDir(), "oberth-hold")
	mode := os.FileMode(0555)
	body := []byte{}
	if kind == "nonexecutable" {
		mode = 0444
	}
	if kind == "replaced" {
		body = []byte("#!/bin/sh\nexit 0\n")
	}
	if kind != "missing" {
		if err = os.WriteFile(binary, body, mode); err != nil {
			t.Fatal(err)
		}
	}
	calls := 0
	pods.ExecFunc = func(ctx context.Context, r argojob.ExecRequest) error {
		calls++
		if r.Command[0] != "/oberth-hold/bin/oberth-hold" {
			t.Fatal("raw exec fallback", r.Command)
		}
		command := exec.CommandContext(ctx, binary, r.Command[1:]...)
		command.Stdout, command.Stderr = r.Out, r.Err
		return command.Run()
	}
	marker := filepath.Join(t.TempDir(), "effect")
	_, err = e.HoldExec(t.Context(), argojob.HoldExecRequest{Binding: b, Hold: holds[0], Command: []string{"touch", marker}})
	if err == nil || calls != 1 {
		t.Fatal("broken helper treated as command result", err, calls)
	}
	if _, err = os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("payload started without the helper", err)
	}
}
