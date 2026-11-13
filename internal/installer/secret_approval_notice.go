package installer

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const SecretApprovalsResetNotice = "all secret approvals are cleared; re-approve."
const SecretApprovalsResetLog = "v0.22.5 secret approval migration: " + SecretApprovalsResetNotice

// Announce only a committed migration reported by the server during this
// operation. Reading its bounded logs adds no database opener or cluster state.
// Dry runs, no-op reruns, and failed migrations cannot claim a reset occurred.
func printSecretApprovalReset(ctx context.Context, deps Deps, namespace string, since time.Time) {
	if deps.RunCommand == nil {
		return
	}
	args := []string{"logs", "--namespace", namespace, "deploy/oberth", "--container", "oberth", "--since-time", since.UTC().Format(time.RFC3339Nano), "--tail=1000"}
	if deps.ContextName != "" {
		args = append([]string{"--context", deps.ContextName}, args...)
	}
	output, err := deps.RunCommand(ctx, nil, "kubectl", args...)
	if err != nil {
		return
	}
	for line := range strings.SplitSeq(string(output), "\n") {
		if strings.HasSuffix(line, SecretApprovalsResetLog) {
			_, _ = fmt.Fprintln(deps.Output, "v0.22.5 secret approval migration:")
			_, _ = fmt.Fprintln(deps.Output, SecretApprovalsResetNotice)
			_, _ = fmt.Fprintln(deps.Output, "After re-approving, run `oberth secretstore sync` to prune the old OpenBao roles and policies.")
			return
		}
	}
}
