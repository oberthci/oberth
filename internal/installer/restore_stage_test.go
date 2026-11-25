package installer

import (
	"bufio"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestRestoreStagesAreAnonymousOnKill(t *testing.T) {
	if os.Getenv("OBERTH_STAGE_TEST_CHILD") == "1" {
		restoreStageChild(t)
		return
	}
	for _, ending := range []string{"kill", "crash"} {
		t.Run(ending, func(t *testing.T) { checkRestoreStageDeath(t, ending) })
	}
}

func restoreStageChild(t *testing.T) {
	t.Helper()
	files, err := newRestoreFiles(stateport.Options{Schema: 6, Protocol: stateport.DefaultProtocol()})
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, file := range []*os.File{files.database, files.snapshot} {
		if _, err = file.Write([]byte("private-stage-fixture")); err != nil {
			t.Fatal(err)
		}
		info, err := file.Stat()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("restore stage mode differs", err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 0 {
			paths = append(paths, "named")
		}
		paths = append(paths, file.Name())
	}
	if err = json.NewEncoder(os.Stdout).Encode(paths); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
	os.Exit(77)
}

func checkRestoreStageDeath(t *testing.T, ending string) {
	t.Helper()
	command := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestRestoreStagesAreAnonymousOnKill$")
	command.Env = append(os.Environ(), "OBERTH_STAGE_TEST_CHILD=1")
	input, output := startStageChild(t, command)
	line, err := output.ReadBytes('\n')
	var paths []string
	if err != nil || json.Unmarshal(line, &paths) != nil {
		t.Fatal("restore stage child did not reach the crash point", err)
	}
	endStageChild(t, command, input, ending)
	for _, path := range paths {
		if path == "named" {
			t.Error("restore stage was named before the crash")
			continue
		}
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			t.Error("decrypted restore stage survived process death", err)
		}
		_ = os.Remove(path)
		if _, err = os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Error("restore staging directory survived process death", err)
		}
		_ = os.Remove(filepath.Dir(path))
	}
}

func startStageChild(t *testing.T, command *exec.Cmd) (io.Closer, *bufio.Reader) {
	t.Helper()
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
	t.Cleanup(func() { _ = command.Process.Kill(); _ = command.Wait() })
	return input, bufio.NewReader(output)
}

// endStageChild kills the child or closes its input, which makes it exit nonzero.
func endStageChild(t *testing.T, command *exec.Cmd, input io.Closer, ending string) {
	t.Helper()
	if ending == "kill" {
		if err := command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = input.Close()
	}
	if err := command.Wait(); err == nil {
		t.Fatal("staging child did not crash")
	}
}
