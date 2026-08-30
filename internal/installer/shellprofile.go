package installer

// Shell-profile wiring: the last manual step of an otherwise complete install.
//
// The install writes an env file and prints "source it, or add it to your
// shell profile". That instruction is the reason a fresh terminal answers
// `oberth status` with "OBERTH_BASE_URL is not set", hours after an install
// that reported success, and it is the reason the first thing an operator
// learns about the CLI is that it does not work.
//
// Appending a line to someone's shell profile is not a thing to do quietly,
// so it is asked for: --shell-profile yes or no answers ahead of time, and an
// interactive install with neither asks. A non-interactive install with
// neither writes nothing and prints the line, which is the old behavior.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// shellProfileMarker identifies the block this installer owns. It is what
// makes a second install find its own work instead of appending beside it, so
// it has to stay stable across versions.
const shellProfileMarker = "# Added by oberth install"

// shellProfileLine is the sourcing line, guarded so a profile still loads
// after the env file is deleted. A shell that cannot start is a much worse
// outcome than a CLI that is not configured.
func shellProfileLine(envPath string) string {
	return fmt.Sprintf("[ -f %q ] && . %q", envPath, envPath)
}

// shellProfilePath picks the file the operator's login shell reads.
//
// $SHELL rather than the parent process, because an install run from inside
// one shell is meant to configure the shell the operator actually logs into.
func shellProfilePath(deps Deps, shell string) (string, error) {
	homeDir := deps.HomeDir
	if homeDir == nil {
		homeDir = os.UserHomeDir
	}
	home, err := homeDir()
	if err != nil {
		return "", err
	}
	switch name := filepath.Base(strings.TrimSpace(shell)); name {
	case "zsh":
		return filepath.Join(home, ".zshrc"), nil
	case "bash":
		// A macOS Terminal window is a login shell, which reads
		// .bash_profile and not .bashrc. Getting this wrong produces a
		// perfectly correct line in a file the operator's terminal never
		// reads, which is indistinguishable from having done nothing.
		if runtime.GOOS == "darwin" {
			return filepath.Join(home, ".bash_profile"), nil
		}
		return filepath.Join(home, ".bashrc"), nil
	case "":
		return "", fmt.Errorf("$SHELL is not set, so there is no profile to write to")
	default:
		return "", fmt.Errorf("shell %q has no profile this installer knows how to edit", name)
	}
}

// appendShellProfile adds the sourcing line once. A profile that already
// carries it is left byte-for-byte alone, so a re-run cannot accumulate
// duplicates and cannot rewrite a file the operator has since edited.
//
// Reports whether anything was written.
func appendShellProfile(path, envPath string) (bool, error) {
	line := shellProfileLine(envPath)
	existing, err := os.ReadFile(path) //nolint:gosec // G304: the path is derived from $SHELL and the home directory, not from a request.
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	// The env path, not the whole line: an operator who wrote their own
	// sourcing line by hand has already solved this, and a second one would
	// be noise at best.
	if strings.Contains(string(existing), envPath) {
		return false, nil
	}

	block := shellProfileMarker + "\n" + line + "\n"
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		block = "\n" + block
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return false, err
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(block); err != nil {
		return false, err
	}
	return true, file.Close()
}

// wireShellProfile settles the choice and does it. It never fails the install:
// a profile that cannot be written is a row and a printed line, because the
// deployment is finished either way.
func wireShellProfile(ctx context.Context, cfg Config, deps Deps, tw *tableWriter, envPath string) {
	choice := strings.ToLower(strings.TrimSpace(cfg.ShellProfile))
	if choice == "" {
		choice = promptShellProfile(ctx, deps, envPath)
	}
	if choice != "yes" {
		tw.AppendRow("Shell profile", "not modified; source "+displayPath(envPath), "— skipped", false)
		return
	}

	path, err := shellProfilePath(deps, os.Getenv("SHELL"))
	if err != nil {
		tw.AppendRow("Shell profile", err.Error(), "⚠ manual", false)
		return
	}
	written, err := appendShellProfile(path, envPath)
	switch {
	case err != nil:
		tw.AppendRow("Shell profile", displayPath(path)+": "+terseInstallError(err), "✗ error", false)
	case written:
		tw.AppendRow("Shell profile", displayPath(path), "✓ wired", false)
	default:
		tw.AppendRow("Shell profile", displayPath(path)+" already sources it", "✓ present", false)
	}
}

// promptShellProfile asks, and answers "no" when there is nobody to ask. A
// script that did not say yes did not consent to an edit of a file it never
// mentioned.
func promptShellProfile(ctx context.Context, deps Deps, envPath string) string {
	if !isInteractive(deps) {
		return "no"
	}
	_, _ = fmt.Fprintf(deps.Output, "\nAdd the Oberth environment to your shell profile, so a new terminal is configured?\n")
	_, _ = fmt.Fprintf(deps.Output, "One line, marked as this installer's: %s\n", shellProfileLine(displayPath(envPath)))
	_, _ = fmt.Fprint(deps.Output, "Add it? [Y/n]: ")
	answer, err := readLine(ctx, deps.Input)
	if err != nil {
		return "no"
	}
	switch strings.ToLower(strings.TrimSpace(answer)) {
	case "", "y", "yes":
		return "yes"
	default:
		return "no"
	}
}
