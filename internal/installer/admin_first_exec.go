package installer

import (
	"context"
	"errors"
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
		starting := strings.Contains(message, "daemon audit mutation gate") && (strings.Contains(message, "no such file") || strings.Contains(message, "connection refused"))
		clear(raw)
		if !starting {
			return nil, errors.New("create first admin failed")
		}
		if err := sleepContext(ctx, pollInterval(deps)); err != nil {
			return nil, errors.Join(errors.New("first-admin daemon gate did not become available"), err)
		}
	}
}
