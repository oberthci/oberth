package installer

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func profileDeps(t *testing.T, home string) Deps {
	t.Helper()
	return Deps{HomeDir: func() (string, error) { return home, nil }}
}

// The install wrote an env file and printed "source it". That instruction is
// why a fresh terminal answers `oberth status` with an unset variable hours
// after an install that reported success.
func TestTheProfileGainsTheSourcingLineOnce(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	profile := filepath.Join(home, ".zshrc")
	envPath := filepath.Join(home, ".config", "oberth", "env")

	written, err := appendShellProfile(profile, envPath)
	if err != nil || !written {
		t.Fatalf("written = %v, err = %v", written, err)
	}
	first, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(first), envPath) || !strings.Contains(string(first), shellProfileMarker) {
		t.Fatalf("the profile does not carry the marked line:\n%s", first)
	}

	// A re-run must find its own work rather than append beside it.
	written, err = appendShellProfile(profile, envPath)
	if err != nil {
		t.Fatal(err)
	}
	if written {
		t.Fatal("a second install appended the line again")
	}
	second, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(second) != string(first) {
		t.Fatalf("a re-run rewrote the profile:\n%s", second)
	}
}

// An operator who already wrote their own sourcing line has solved this, and a
// second one would be noise.
func TestAHandWrittenSourcingLineIsLeftAlone(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	profile := filepath.Join(home, ".zshrc")
	envPath := filepath.Join(home, ".config", "oberth", "env")
	body := "source " + envPath + "\n"
	if err := os.WriteFile(profile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	written, err := appendShellProfile(profile, envPath)
	if err != nil || written {
		t.Fatalf("written = %v, err = %v; want the operator's own line respected", written, err)
	}
	after, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != body {
		t.Fatalf("the profile changed:\n%s", after)
	}
}

// A profile whose last line has no newline must not gain a line glued to it.
func TestAProfileWithoutATrailingNewlineIsNotCorrupted(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	profile := filepath.Join(home, ".zshrc")
	if err := os.WriteFile(profile, []byte("export PATH=/usr/bin"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := appendShellProfile(profile, filepath.Join(home, "env")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(profile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "export PATH=/usr/bin\n") {
		t.Fatalf("the existing last line was joined to the new one:\n%s", body)
	}
}

// A macOS Terminal window is a login shell, which reads .bash_profile. A
// correct line in a file the terminal never reads is doing nothing.
func TestTheProfileIsChosenFromTheLoginShell(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	deps := profileDeps(t, home)

	zsh, err := shellProfilePath(deps, "/bin/zsh")
	if err != nil || filepath.Base(zsh) != ".zshrc" {
		t.Fatalf("zsh profile = %q, err = %v", zsh, err)
	}
	bash, err := shellProfilePath(deps, "/bin/bash")
	if err != nil {
		t.Fatal(err)
	}
	wantBash := ".bashrc"
	if runtime.GOOS == "darwin" {
		wantBash = ".bash_profile"
	}
	if filepath.Base(bash) != wantBash {
		t.Fatalf("bash profile = %q, want %s", bash, wantBash)
	}
	// A shell this installer cannot edit safely says so rather than writing
	// bourne syntax into a fish config.
	if _, err := shellProfilePath(deps, "/usr/local/bin/fish"); err == nil {
		t.Fatal("an unknown shell was accepted")
	}
	if _, err := shellProfilePath(deps, ""); err == nil {
		t.Fatal("an unset $SHELL was accepted")
	}
}

// A script that did not say yes did not consent to an edit of a file it never
// mentioned.
func TestANonInteractiveInstallDoesNotTouchTheProfile(t *testing.T) {
	t.Parallel()
	deps := Deps{Output: &strings.Builder{}}
	if got := promptShellProfile(context.Background(), deps, "/tmp/env"); got != "no" {
		t.Fatalf("choice = %q, want no", got)
	}
}

// A typo that silently declines produces exactly the behavior the flag was
// passed to avoid.
func TestAnUnknownShellProfileAnswerIsRefused(t *testing.T) {
	t.Parallel()
	cfg := Config{ShellProfile: "true"}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "yes or no") {
		t.Fatalf("err = %v, want a usage error", err)
	}
	for _, accepted := range []string{"", "yes", "no", " YES "} {
		cfg := Config{ShellProfile: accepted}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("--shell-profile %q was refused: %v", accepted, err)
		}
	}
}
