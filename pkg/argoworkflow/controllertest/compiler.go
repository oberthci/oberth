// Package controllertest shares compiled pinned controller oracles between test cases.
// Spec: docs/plans/c4-admission-rules-and-plan.md#purpose
package controllertest

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

//nolint:gochecknoglobals // The pinned dependency location is immutable for the test process.
var moduleCache struct {
	sync.Mutex
	path string
}

// Module resolves the pinned source location once for the test process.
func Module(ctx context.Context, t *testing.T) string {
	t.Helper()
	moduleCache.Lock()
	defer moduleCache.Unlock()
	if moduleCache.path == "" {
		command := exec.CommandContext(ctx, "go", "list", "-p=2", "-buildvcs=false", "-m", "-f", "{{.Dir}}", "github.com/argoproj/argo-workflows/v4")
		raw, err := command.Output()
		if err != nil {
			t.Fatal(err)
		}
		moduleCache.path = strings.TrimSpace(string(raw))
	}
	return moduleCache.path
}

// Fixture owns one read-only executable and its private build directory.
type Fixture struct {
	mu        sync.Mutex
	files     map[string]string
	directory string
	result    func() (string, error)
}

// New maps controller test basenames to the caller's source fixture files.
func New(files map[string]string) *Fixture {
	return &Fixture{files: maps.Clone(files)}
}

// binaryPath compiles the controller with race instrumentation once per fixture.
func (f *Fixture) binaryPath(ctx context.Context, t *testing.T) string {
	t.Helper()
	binary, err := f.compilation(ctx, Module(ctx, t))
	if err != nil {
		t.Fatal(err)
	}
	return binary
}

func (f *Fixture) compilation(ctx context.Context, module string) (string, error) {
	f.mu.Lock()
	if f.result == nil {
		f.result = sync.OnceValues(func() (string, error) { return f.compile(ctx, module) })
	}
	result := f.result
	f.mu.Unlock()
	return result()
}

func (f *Fixture) compile(ctx context.Context, module string) (string, error) {
	directory, err := os.MkdirTemp(os.Getenv("GOTMPDIR"), "oberth-controller-test-")
	if err != nil {
		return "", err
	}
	f.directory = directory
	native := filepath.Join(f.directory, "native")
	if err := os.CopyFS(native, os.DirFS(module)); err != nil {
		return "", err
	}
	for name, source := range f.files {
		raw, err := os.ReadFile(source)
		if err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(native, "workflow/controller", name), raw, 0600); err != nil {
			return "", err
		}
	}
	binary := filepath.Join(f.directory, "controller.test")
	command := exec.CommandContext(ctx, "go", "test", "-c", "-race", "-ldflags=-w", "-mod=readonly", "-p=1", "-buildvcs=false", "-timeout=180s", "-o", binary, "./workflow/controller")
	command.Dir = native
	if raw, err := Output(command); err != nil {
		return "", fmt.Errorf("compile controller oracle: %w\n%s", err, raw)
	}
	if err := os.Chmod(binary, 0555); err != nil {
		return "", err
	}
	return binary, nil
}

// Command runs the immutable oracle with Go's premature-success check enabled.
func (f *Fixture) Command(ctx context.Context, t *testing.T, args ...string) *exec.Cmd {
	t.Helper()
	binary := f.binaryPath(ctx, t)
	command := exec.CommandContext(ctx, binary, append([]string{"-test.paniconexit0"}, args...)...)
	command.Dir = filepath.Join(f.directory, "native", "workflow/controller")
	return command
}

// Close removes the build directory after every test has joined.
func (f *Fixture) Close() error {
	if f.directory == "" {
		return nil
	}
	return os.RemoveAll(f.directory)
}

// Output joins the command and kills its owned process group on cancellation.
func Output(command *exec.Cmd) ([]byte, error) {
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	command.WaitDelay = time.Second
	defer func() {
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
	}()
	return command.CombinedOutput()
}
