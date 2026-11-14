package installer

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Retry only an unavailable daemon socket during startup, using the persisted
// enrollment request. Credential refusals and interrupted responses return to
// the operator; the same key remains available for a later resume.
func runFirstAdminExec(ctx context.Context, deps Deps, body []byte, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	for {
		raw, err := deps.RunCommand(ctx, body, "kubectl", args...)
		if err == nil {
			return raw, nil
		}
		message := string(raw) + " " + err.Error()
		starting := (strings.Contains(message, "daemon audit mutation gate") || strings.Contains(message, "inspect admin socket directory")) && (strings.Contains(message, "no such file") || strings.Contains(message, "connection refused"))
		failure := firstAdminExecError(err)
		clear(raw)
		if !starting {
			return nil, failure
		}
		if err := sleepContext(ctx, pollInterval(deps)); err != nil {
			return nil, errors.Join(errors.New("first-admin daemon gate did not become available"), err)
		}
	}
}

// Only stderr is diagnostic. A lost stdout response may contain credentials.
func firstAdminExecError(err error) error {
	var exit *exec.ExitError
	if errors.As(err, &exit) && len(exit.Stderr) > 0 {
		message := exit.Stderr
		if len(message) > 2048 {
			message = message[:2048]
		}
		return fmt.Errorf("create first admin refused: %s", strings.TrimSpace(string(message)))
	}
	return fmt.Errorf("create first admin failed: %w", err)
}
