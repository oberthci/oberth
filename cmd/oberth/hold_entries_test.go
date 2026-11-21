package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestHeldExecutableEntriesWithoutProfile(t *testing.T) {
	t.Parallel()
	binary := composeBinary(t)
	destination := t.TempDir()
	out, detail, code := heldExecutable(t, binary, []string{"internal-hold", "install", "--to", destination})
	if code != 0 || out != "" || detail != "" {
		t.Fatal("helper installation failed", code, out, detail)
	}
	helper := filepath.Join(destination, "oberth-hold")
	info, err := os.Stat(helper)
	if err != nil || info.Mode().Perm() != 0555 {
		t.Fatal("helper is not executable", info, err)
	}
	for _, test := range []struct {
		args []string
		code int
	}{
		{[]string{"run", "--", "true"}, 126},
		{[]string{"continue"}, 120},
		{[]string{"patch"}, 120},
		{[]string{"exec", "--stdin", "unexpected"}, 120},
	} {
		out, detail, code = heldExecutable(t, helper, test.args)
		if code != test.code || out != "" || !strings.HasPrefix(detail, "oberth-hold:") {
			t.Fatal("helper entry changed result or used CLI profile", test.args, code, out, detail)
		}
	}
}
func heldExecutable(t *testing.T, binary string, args []string) (string, string, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, args...)
	command.Env = []string{"HOME=", "XDG_CONFIG_HOME="}
	command.Stdin = strings.NewReader("ignored input")
	var out, detail bytes.Buffer
	command.Stdout, command.Stderr = &out, &detail
	command.WaitDelay = 5 * time.Second
	err := command.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), detail.String(), exit.ExitCode()
	}
	if err != nil {
		t.Fatal("helper process failed", err)
	}
	return out.String(), detail.String(), 0
}
func TestHeldCLIContractDropsOldActionsAndStdin(t *testing.T) {
	for _, words := range [][]string{{"run", "continue"}, {"run", "hold", "patch"}, {"run", "hold", "abandon"}} {
		if _, _, found := findCommand(commands(), words); found {
			t.Fatal("removed command advertised", words)
		}
	}
	for _, name := range []string{"list", "exec", "release"} {
		if _, _, found := findCommand(commands(), []string{"run", "hold", name}); !found {
			t.Fatal("landed hold command missing", name)
		}
	}
	row, _, _ := findCommand(commands(), []string{"run", "hold", "exec"})
	for _, flag := range []string{"--stdin", "--patch-root"} {
		_, problems := toolArgs(row, []string{"--id", "12345678", "--argv", `["pwd"]`, flag, "value"}, strings.NewReader(""), nil)
		if len(problems) == 0 {
			t.Fatal("removed plumbing accepted", flag)
		}
	}
}
