package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestBinaryStderrContainsOnlyProductDiagnostics(t *testing.T) {
	t.Parallel()
	binary := composeBinary(t)
	cmd := exec.CommandContext(t.Context(), binary, "tool", "run_get", "-")
	var out, diagnostic bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &diagnostic
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = input.Close()
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	<-timer.C
	if err = cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	timer.Reset(100 * time.Millisecond)
	<-timer.C
	_, _ = input.Write([]byte("{"))
	_ = input.Close()
	_ = cmd.Wait()
	if strings.Contains(diagnostic.String(), "Hydrator config") || strings.Contains(diagnostic.String(), `"level"`) || out.Len() != 0 {
		t.Fatalf("dependency wrote command diagnostics: %s / %s", out.String(), diagnostic.String())
	}
	if !strings.Contains(diagnostic.String(), "Command arguments are invalid.") {
		t.Fatal("product refusal missing", diagnostic.String())
	}
}
