package installer

import (
	"context"
	"fmt"
)

// recordServerAudit appends a host lifecycle event through the authenticated API.
// The host operation has already succeeded, so failure prints the audit gap.
func recordServerAudit(ctx context.Context, deps Deps, namespace, action string, details map[string]any) {
	err := CallAdmin(ctx, Config{Namespace: namespace}, deps, "audit-record", map[string]any{"action": action, "details": details}, nil)
	if err != nil {
		_, _ = fmt.Fprintf(deps.Output, "  could not record %s in the server audit chain: %v\n", action, err)
		return
	}
	_, _ = fmt.Fprintf(deps.Output, "  recorded %s in the server audit chain\n", action)
}
