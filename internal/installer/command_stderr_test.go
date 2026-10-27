package installer

import (
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func TestDefaultRunCommandRetainsStderrSeparately(t *testing.T) {
	t.Parallel()
	for _, stream := range []string{"stdout", "stderr"} {
		t.Run(stream, func(t *testing.T) {
			// Fixed test shell, not an operator-provided command. Both streams are
			// returned for diagnostics, but only stderr can classify a legacy server.
			script := `printf '%s' 'unknown command "admin"'; printf '%s' 'permission denied' >&2; exit 2`
			if stream == "stderr" {
				script = `printf '%s' 'certificate stdout'; printf '%s' 'unknown command "admin"' >&2; exit 2`
			}
			body, err := DefaultRunCommand(t.Context(), nil, "/bin/sh", "-c", script)
			var exitError *exec.ExitError
			if !errors.As(err, &exitError) || exitError.ExitCode() != 2 {
				t.Fatalf("missing child exit status: %v", err)
			}
			if !strings.Contains(string(body), `unknown command "admin"`) {
				t.Fatalf("combined diagnostics lost: %q", body)
			}
			matches := strings.Contains(string(exitError.Stderr), `unknown command "admin"`)
			if matches != (stream == "stderr") {
				t.Fatalf("stderr mixed with stdout: %q", exitError.Stderr)
			}
		})
	}
}
