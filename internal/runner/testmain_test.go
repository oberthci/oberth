package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	// TestMain builds ./testdata/jq as a separate command. The blank import
	// keeps that helper's module visible to go mod tidy, which ignores testdata.
	_ "github.com/itchyny/gojq"
)

const (
	runnerTestJQDirectoryEnv     = "OBERTH_TEST_JQ_DIRECTORY"
	runnerTestPythonDirectoryEnv = "OBERTH_TEST_PYTHON_DIRECTORY"
)

func TestMain(m *testing.M) {
	directory, err := os.MkdirTemp("", "oberth-test-jq-")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "create hermetic jq directory: %v\n", err)
		os.Exit(1)
	}
	goBinary, err := exec.LookPath("go")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "find Go for hermetic jq: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	pythonBinary, err := exec.LookPath("python3")
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "find repository-installed Python for runner tests: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	pythonDirectory, err := absoluteExecutableDirectory(pythonBinary)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "normalize repository-installed Python path: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	wrapper := filepath.Join(directory, "jq")
	build := exec.Command(goBinary, "build", "-buildvcs=false", "-trimpath", "-o", wrapper, "./testdata/jq")
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "build hermetic jq: %v\n%s", err, output)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	previousPath := os.Getenv("PATH")
	if err := os.Setenv(runnerTestJQDirectoryEnv, directory); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "record hermetic jq path: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	if err := os.Setenv(runnerTestPythonDirectoryEnv, pythonDirectory); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "record repository-installed Python path: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}
	if err := os.Setenv("PATH", directory+string(os.PathListSeparator)+previousPath); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "install hermetic jq path: %v\n", err)
		_ = os.RemoveAll(directory)
		os.Exit(1)
	}

	status := m.Run()
	_ = os.Setenv("PATH", previousPath)
	_ = os.Unsetenv(runnerTestJQDirectoryEnv)
	_ = os.Unsetenv(runnerTestPythonDirectoryEnv)
	_ = os.RemoveAll(directory)
	os.Exit(status)
}

func absoluteExecutableDirectory(executable string) (string, error) {
	return filepath.Abs(filepath.Dir(executable))
}

func TestAbsoluteExecutableDirectoryNormalizesRelativeLookup(t *testing.T) {
	want, err := filepath.Abs(filepath.Join("relative-path-entry", "bin"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := absoluteExecutableDirectory(filepath.Join("relative-path-entry", "bin", "python3"))
	if err != nil {
		t.Fatal(err)
	}
	if got != want || !filepath.IsAbs(got) {
		t.Fatalf("absolute executable directory = %q, want %q", got, want)
	}
}
