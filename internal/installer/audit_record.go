package installer

import (
	"context"
	"encoding/json"
	"fmt"
)

// recordServerAudit asks the running server pod to append one lifecycle action
// to its audit chain (`oberth audit record`, #1104). Backup and restore run on
// the workstation, so the server learns of them only through this call. The
// operation has already succeeded by then, so a failure (an older server
// without the subcommand, an unreachable admin daemon) is a warning, never an
// error: the gap is stated, not hidden.
func recordServerAudit(ctx context.Context, deps backupDeps, namespace, pod, action string, details map[string]any) {
	encoded, err := json.Marshal(details)
	if err == nil {
		_, err = deps.RunCommand(ctx, nil, "kubectl", kubectlExecArgs(deps.ContextName, namespace, pod, serverContainerName, false,
			"oberth", "audit", "record", action, "--details", string(encoded))...)
	}
	if err != nil {
		_, _ = fmt.Fprintf(deps.Output, "  ⚠ could not record %s in the server's audit chain: %v\n", action, err)
		return
	}
	_, _ = fmt.Fprintf(deps.Output, "  ✓ recorded %s in the server's audit chain\n", action)
}
