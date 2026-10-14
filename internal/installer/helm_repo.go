package installer

import (
	"context"
	"fmt"
)

// refreshHelmRepo registers the repo and refreshes its index before resolving
// charts. Helm 4's repo add --force-update can leave an existing index stale.
func refreshHelmRepo(ctx context.Context, deps Deps, name, url string) error {
	if _, err := deps.RunHelm(ctx, []string{"repo", "add", "--force-update", name, url}); err != nil {
		return err
	}
	if _, err := deps.RunHelm(ctx, []string{"repo", "update", name}); err != nil {
		return fmt.Errorf("refresh Helm repo %s: %w", name, err)
	}
	return nil
}
