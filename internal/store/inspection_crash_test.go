//go:build linux

package store

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/stateport"
)

func TestInspectionHasNoPlaintextFilesOnProcessDeath(t *testing.T) {
	if os.Getenv("OBERTH_INSPECT_TEST_CHILD") == "1" {
		inspectionCrashChild(t)
		return
	}
	source := testStore(t)
	options := backupOptions(source)
	var archive bytes.Buffer
	if _, err := source.Snapshot(t.Context(), options, &archive); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "fixture.tar")
	if err := os.WriteFile(path, archive.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	for _, ending := range []string{"kill", "crash"} {
		t.Run(ending, func(t *testing.T) { inspectProcessDeath(t, path, options, ending) })
	}
}

type inspectionDeathReader struct{ io.Reader }

func (r inspectionDeathReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	if err == io.EOF {
		_, _ = fmt.Fprintln(os.Stdout, "inspection-ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(77)
	}
	return n, err
}

func inspectionCrashChild(t *testing.T) {
	t.Helper()
	var options stateport.Options
	if err := json.Unmarshal([]byte(os.Getenv("OBERTH_INSPECT_TEST_OPTIONS")), &options); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(os.Getenv("OBERTH_INSPECT_TEST_ARCHIVE"))
	if err != nil {
		t.Fatal(err)
	}
	backup, err := NewBackup(filepath.Join(os.Getenv("OBERTH_INSPECT_TEST_PARENT"), "inspect.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = backup.Inspect(t.Context(), options, inspectionDeathReader{bytes.NewReader(raw)}); err != nil {
		t.Fatal(err)
	}
	t.Fatal("inspection never reached its crash point")
}

func inspectProcessDeath(t *testing.T, archive string, options stateport.Options, ending string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	raw, err := json.Marshal(options)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestInspectionHasNoPlaintextFilesOnProcessDeath$")
	command.Env = append(os.Environ(), "OBERTH_INSPECT_TEST_CHILD=1", "OBERTH_INSPECT_TEST_ARCHIVE="+archive,
		"OBERTH_INSPECT_TEST_PARENT="+t.TempDir(), "OBERTH_INSPECT_TEST_OPTIONS="+string(raw))
	input, output := startInspectionChild(t, command)
	ready, err := output.ReadString('\n')
	if err != nil || ready != "inspection-ready\n" {
		t.Fatal("inspection did not reach its crash point", err)
	}
	paths := liveInspectionFiles(t, command.Process.Pid)
	endInspectionChild(t, command, input, ending)
	for _, path := range paths {
		if _, err = os.Lstat(path); !os.IsNotExist(err) {
			t.Error("decrypted inspection file survived process death", err)
		}
		if _, err = os.Lstat(filepath.Dir(path)); !os.IsNotExist(err) {
			t.Error("inspection stage survived process death", err)
		}
		_ = os.RemoveAll(filepath.Dir(path))
	}
}

func liveInspectionFiles(t *testing.T, pid int) []string {
	t.Helper()
	root := fmt.Sprintf("/proc/%d/fd", pid)
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range entries {
		fd := filepath.Join(root, entry.Name())
		path, err := os.Readlink(fd)
		if err != nil {
			continue
		}
		path = strings.TrimSuffix(path, " (deleted)")
		if !strings.Contains(path, string(os.PathSeparator)+stagePrefix) || filepath.Base(path) != "oberth.sqlite" {
			continue
		}
		paths = append(paths, path)
		info, err := os.Stat(fd)
		if err != nil {
			t.Fatal(err)
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Nlink != 0 || info.Mode().Perm() != 0600 {
			t.Error("decrypted inspection data still has a pathname")
		}
	}
	if len(paths) != 1 {
		t.Fatal("inspection did not hold exactly one anonymous source file", len(paths))
	}
	return paths
}

func startInspectionChild(t *testing.T, command *exec.Cmd) (io.Closer, *bufio.Reader) {
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

// endInspectionChild kills the child or closes its input, which makes it exit nonzero.
func endInspectionChild(t *testing.T, command *exec.Cmd, input io.Closer, ending string) {
	t.Helper()
	if ending == "kill" {
		if err := command.Process.Kill(); err != nil {
			t.Fatal(err)
		}
	} else {
		_ = input.Close()
	}
	if err := command.Wait(); err == nil {
		t.Fatal("inspection child did not crash")
	}
}
