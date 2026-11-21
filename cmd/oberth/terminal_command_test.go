package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/apicontract"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/stepmodel"
)

func TestTerminalCommandBoundaries(t *testing.T) {
	if len(os.Args) >= 2 && os.Args[len(os.Args)-2] == "terminal-command-child" {
		runTerminalCommandChild(t, os.Args[len(os.Args)-1])
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"secretstore", "log", "issue"} {
		t.Run(kind, func(t *testing.T) {
			argv := []string{binary, "-test.run=^TestTerminalCommandBoundaries$", "--", "terminal-command-child", kind}
			ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
			defer cancel()
			command := terminalTestCommand(ctx, argv)
			output, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("terminal command failed: %v output=%q", err, output)
			}
			want := "\x1b[32mstep\x1b[0m\\x1b]0;title\\a\\r\\x1b[2J"
			if kind == "secretstore" {
				want = "\x1b[32mstep\x1b[0m\x1b]0;title\a\r\x1b[2J"
			}
			if !strings.Contains(string(output), want) {
				t.Fatalf("terminal output=%q, want %q", output, want)
			}
		})
	}
}

func terminalTestCommand(ctx context.Context, argv []string) *exec.Cmd {
	args := append([]string{"-q", "/dev/null"}, argv...)
	if runtime.GOOS == "linux" {
		quoted := make([]string, len(argv))
		for i, arg := range argv {
			quoted[i] = shellArgument(arg)
		}
		args = []string{"-q", "-e", "-c", strings.Join(quoted, " "), "/dev/null"}
	}
	command := exec.CommandContext(ctx, "script", args...)
	command.WaitDelay = 5 * time.Second
	return command
}

func runTerminalCommandChild(t *testing.T, kind string) {
	t.Helper()
	text := "\x1b[32mstep\x1b[0m\x1b]0;title\a\r\x1b[2J"
	b := fake.Bindings()
	b.RunGet.Handler = &fake.Handler[apicontract.RunGetArgs, apicontract.RunRecord]{Value: apicontract.RunRecord{RunSummary: stepmodel.RunSummary{ID: "run", Status: stepmodel.RunPassed, Steps: []stepmodel.RunStep{{Path: "build", Name: "build", Status: stepmodel.StepPassed}}}}}
	b.RunLogs.Handler = &fake.Handler[apicontract.RunLogsArgs, apicontract.LogResult]{Value: apicontract.LogResult{Content: text}}
	b.IssueGet.Handler = &fake.Handler[apicontract.IssueGetArgs, model.Issue]{Value: model.Issue{ID: 1, Title: "title", Body: text}}
	app, _, _ := fixture(t, b)
	app.in, app.out, app.diagnostic = os.Stdin, os.Stdout, os.Stderr
	app.secretExec = func(ctx context.Context, request installationRequest, in io.Reader, out io.Writer) error {
		command := exec.CommandContext(ctx, request.args[0], request.args[1:]...)
		command.Stdin, command.Stdout, command.Stderr = in, out, os.Stderr
		command.WaitDelay = 5 * time.Second
		return command.Run()
	}
	args := []string{"log", "run", "build"}
	switch kind {
	case "issue":
		args = []string{"issue", "get", "1"}
	case "secretstore":
		args = []string{"secretstore", "exec", "--", "sh", "-c", `test -t 0 && test -t 1 && test -t 2 || exit 19; printf '\033[32mstep\033[0m\033]0;title\007\r\033[2J'`}
	}
	if code := app.run(t.Context(), args); code != 0 {
		t.Fatalf("terminal command exit=%d", code)
	}
}
