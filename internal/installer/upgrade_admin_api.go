package installer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/mod/semver"

	"github.com/oberthci/oberth/internal/client"
)

// A server without these routes must be backed up with its own released CLI.
// The new CLI never substitutes a server exec for the authenticated API.
func checkUpgradeBackupAPI(ctx context.Context, cfg UpgradeConfig, deps Deps, currentVersion string) error {
	if cfg.NoBackup {
		return nil
	}
	if semver.Compare(canonicalChartVersion(currentVersion), "v0.22.7") < 0 {
		return missingUpgradeAdminAPI(cfg, deps, currentVersion)
	}
	_, _, err := adminVersion(ctx, Config{Namespace: cfg.Namespace}, deps)
	var status *client.AdminError
	if errors.As(err, &status) && status.StatusCode == http.StatusNotFound {
		return fmt.Errorf("administration API missing from running release %s; pre-upgrade backup refused before any change; a failed release may still run older Pods. After taking a backup with the CLI matching those Pods, rerun:\n\n  %s\n\n%s", displayVersion(currentVersion), upgradeRetryCommand(cfg, deps), upgradeRetryOptionsNote)
	}
	return nil
}

func missingUpgradeAdminAPI(cfg UpgradeConfig, deps Deps, currentVersion string) error {
	version := displayVersion(currentVersion)
	if deps.ContextName == "" {
		return fmt.Errorf("running release %s has no admin API; select its Kubernetes context explicitly and retry for the matching backup command; confirm no runs are active before retrying with --no-wait-idle", version)
	}
	binary := "./oberth-" + version
	target := " --context " + shellQuoteWord(deps.ContextName) + " --namespace " + shellQuoteWord(cfg.Namespace)
	retry := upgradeRetryCommand(cfg, deps)
	return fmt.Errorf("running release %s has no administration API; pre-upgrade backup refused before any change. Use the verified %s CLI saved as %s, then run:\n\n  %s backup --history-only%s\n  %s\n\nIf Kubernetes Workflow observation is unavailable, confirm no runs are active before retrying with --no-wait-idle. No exec fallback is used. %s", version, version, strings.TrimPrefix(binary, "./"), shellQuoteWord(binary), target, retry, upgradeRetryOptionsNote)
}

const upgradeRetryOptionsNote = "Reapply your other upgrade options: --openbao-namespace, --values/--set, --backup-dir, --timeout, TLS flags, chart/release-source flags and downgrade consent are not copied into this retry command."

func upgradeRetryCommand(cfg UpgradeConfig, deps Deps) string {
	retry := "oberth upgrade --no-backup --context " + shellQuoteWord(deps.ContextName) + " --namespace " + shellQuoteWord(cfg.Namespace)
	if cfg.NoWaitIdle {
		retry += " --no-wait-idle"
	}
	if cfg.WaitTimeout > 0 && cfg.WaitTimeout != DefaultUpgradeWaitTimeout {
		retry += " --wait-for " + cfg.WaitTimeout.String()
	}
	if cfg.ArgoSecurityDefaults {
		retry += " --argo-security-defaults"
	}
	return retry
}

func checkUpgradeVerificationConnection(ctx context.Context, cfg UpgradeConfig, deps Deps) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, closeConnection, err := adminAPI(ctx, Config{Namespace: cfg.Namespace}, deps)
	closeConnection()
	if err != nil {
		return fmt.Errorf("upgrade requires this machine's client connected to this server as an unscoped admin; configure it with `export OBERTH_BASE_URL='https://<server>:<port>' OBERTH_CLIENT_CERT='/path/admin.crt' OBERTH_CLIENT_KEY='/path/admin.key' OBERTH_CA_CERT='/path/server-ca.crt'`, then retry (nothing changed): %w", err)
	}
	return nil
}
