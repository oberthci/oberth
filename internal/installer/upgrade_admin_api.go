package installer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/oberthci/oberth/internal/client"
	"golang.org/x/mod/semver"
)

// A server without these routes must be backed up with its own released CLI.
// The new CLI never substitutes a server exec for the authenticated API.
func checkUpgradeBackupAPI(ctx context.Context, cfg UpgradeConfig, deps Deps, currentVersion string) error {
	if cfg.NoBackup || cfg.DryRun {
		return nil
	}
	_, _, err := adminVersion(ctx, Config{Namespace: cfg.Namespace}, deps)
	var status *client.AdminError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		if semver.Compare(canonicalChartVersion(currentVersion), "v0.22.7") < 0 {
			return missingUpgradeAdminAPI(cfg, deps, currentVersion)
		}
		return fmt.Errorf("administration API missing from running release %s; pre-upgrade backup refused before any change; verify the running image and selected endpoint", displayVersion(currentVersion))
	}
	return nil
}

func missingUpgradeAdminAPI(cfg UpgradeConfig, deps Deps, currentVersion string) error {
	version := displayVersion(currentVersion)
	if deps.ContextName == "" {
		return fmt.Errorf("running release %s has no admin API; select its Kubernetes context explicitly and retry for the matching backup command; confirm no runs are active before upgrading without idle observation", version)
	}
	binary := "./oberth-" + version
	target := " --context " + shellQuoteWord(deps.ContextName) + " --namespace " + shellQuoteWord(cfg.Namespace)
	retry := "oberth upgrade --no-backup" + target
	if cfg.WaitIdle {
		if cfg.WaitFor == "critical" {
			retry += " --wait-for critical"
		} else {
			retry += " --wait-idle"
		}
		if cfg.WaitTimeout > 0 {
			retry += " --wait-timeout " + cfg.WaitTimeout.String()
		}
	}
	if cfg.ArgoSecurityDefaults {
		retry += " --argo-security-defaults"
	}
	return fmt.Errorf("running release %s has no administration API; pre-upgrade backup refused before any change. Use the verified %s CLI saved as %s, then run:\n\n  %s backup --history-only%s\n  %s\n\nIf Kubernetes Workflow observation is unavailable, confirm no runs are active before retrying without --wait-idle or --wait-for. No exec fallback is used", version, version, strings.TrimPrefix(binary, "./"), shellQuoteWord(binary), target, retry)
}
