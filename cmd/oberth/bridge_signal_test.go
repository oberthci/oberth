package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/api/fake"
	"github.com/oberthci/oberth/internal/client"
)

func TestAgentSignalStopsBridge(t *testing.T) {
	if os.Getenv("OBERTH_TEST_AGENT_SIGNAL") == "1" {
		agentSignalChild(t)
		return
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []os.Signal{os.Interrupt, syscall.SIGTERM} {
		checkAgentSignal(t, binary, signal)
	}
}

func checkAgentSignal(t *testing.T, binary string, signal os.Signal) {
	t.Helper()
	command := exec.CommandContext(t.Context(), binary, "-test.run=^TestAgentSignalStopsBridge$", "-test.timeout=15s")
	command.Env = append(os.Environ(), "OBERTH_TEST_AGENT_SIGNAL=1")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = command.Start(); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(input).Encode(modernMCPFrame(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "server/discover"})); err != nil {
		t.Fatal(err)
	}

	scanner := bufio.NewScanner(output)
	if !scanner.Scan() || !strings.Contains(scanner.Text(), `"result"`) {
		t.Fatal("agent did not discover", scanner.Text())
	}
	if err = command.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	if !scanner.Scan() || scanner.Text() != "closed" {
		t.Fatal("signal did not stop bridge", scanner.Text())
	}
	_ = input.Close()
	if err = command.Wait(); err != nil {
		t.Fatal(err)
	}
}

func agentSignalChild(t *testing.T) {
	app, _, _ := fixture(t, fake.Bindings())
	app.base = t.TempDir()
	dir := filepath.Join(app.base, "agent")
	if err := os.Chmod(app.base, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, data := range map[string]string{"installation.json": `{"server_url":"https://127.0.0.1","uplink":true}`, "token": "valid"} {
		if err := client.WritePrivate(filepath.Join(dir, name), []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	ctx, stop := interruptContext(t.Context())
	defer stop()
	// Keep stdout open to report that cancellation joined the bridge.
	app.in, app.out = os.Stdin, struct{ io.Writer }{os.Stdout}
	app.runMCP(ctx, "agent")
	_, _ = fmt.Fprintln(os.Stdout, "closed")
}
