package installer

import (
	"context"
	"fmt"
)

// recordServerAudit asks the running server pod to append one lifecycle action
// to its audit chain (`oberth audit record`, #1104). Backup, restore and upgrade run on
// the workstation, so the server learns of them only through this call. The
// operation has already succeeded by then, so a failure (an older server
// without the subcommand, an unreachable admin daemon) is a warning, never an
// error: the gap is stated, not hidden.
func recordServerAudit(ctx context.Context, deps Deps, namespace, pod, action string, details map[string]any) {
	err := CallAdmin(ctx, Config{Namespace: namespace}, deps, "audit-record", map[string]any{"action": action, "details": details}, nil)
	if err != nil {
		_, _ = fmt.Fprintf(deps.Output, "  could not record %s in the server audit chain: %v\n", action, err)
		return
	}
	_, _ = fmt.Fprintf(deps.Output, "  recorded %s in the server audit chain\n", action)
}
