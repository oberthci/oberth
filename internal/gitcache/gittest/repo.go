package gittest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fixture contains a bare store and an independent author worktree.
type Fixture struct{ Root, Work, Bare, Binary, Commit string }

func New(t *testing.T) *Fixture { return NewFormat(t, "sha1") }

// NewFormat uses the Git named by OBERTH_TEST_GIT, the packaged Git the product
// runs, and otherwise the first git on PATH.
func NewFormat(t *testing.T, format string) *Fixture {
	t.Helper()
	binary := os.Getenv("OBERTH_TEST_GIT")
	var err error
	if binary == "" {
		binary, err = exec.LookPath("git")
	}
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	f := &Fixture{
		Root:   root,
		Work:   filepath.Join(root, "author"),
		Bare:   filepath.Join(root, "upstream.git"),
		Binary: binary,
	}
	if err = os.MkdirAll(f.Work, 0700); err != nil {
		t.Fatal(err)
	}
	f.Git(t, f.Work, "init", "--initial-branch=main", "--object-format="+format)
	f.Git(t, f.Work, "config", "user.name", "Oberth")
	f.Git(t, f.Work, "config", "user.email", "ci@oberth.ci")
	f.Write(t, "file.txt", []byte("exact source\n"))
	f.Commit = f.CommitFile(t, "initial")
	f.Git(t, f.Work, "clone", "--bare", f.Work, f.Bare)
	return f
}
// FixedDate is the author and committer date of every fixture commit made
// without an explicit date, so fixture object IDs are the same on every run.
const FixedDate = "2000-01-01T00:00:00Z"

func (f *Fixture) Git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return f.git(t, FixedDate, dir, args...)
}
func (f *Fixture) git(t *testing.T, date, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(f.Binary, append([]string{"-c", "core.hooksPath=/dev/null", "-c", "gc.autoDetach=false", "-c", "maintenance.autoDetach=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=/nonexistent",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_DATE=" + date,
		"GIT_COMMITTER_DATE=" + date,
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
func (f *Fixture) Write(t *testing.T, name string, body []byte) {
	t.Helper()
	p := filepath.Join(f.Work, name)
	if err := os.MkdirAll(filepath.Dir(p), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, body, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *Fixture) CommitFile(t *testing.T, message string) string {
	t.Helper()
	return f.CommitFileAt(t, message, FixedDate)
}

// CommitFileAt commits the work tree with date as its author and committer date.
func (f *Fixture) CommitFileAt(t *testing.T, message, date string) string {
	t.Helper()
	f.Git(t, f.Work, "add", ".")
	f.git(t, date, f.Work, "commit", "-m", message)
	return f.Git(t, f.Work, "rev-parse", "HEAD")
}
