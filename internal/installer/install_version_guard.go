package installer

import (
	"context"
	"fmt"
	"strings"

	"golang.org/x/mod/semver"
)

// preflightInstallVersionMove refuses, before anything is installed, an
// install or install --upgrade rerun that would move an existing Oberth
// release to another chart version. Same-version reconciliation (adding a
// secret store, Rekor, TLS names or Argo limits) stays with install; version
// changes go through oberth upgrade, the one path with signed release
// verification, a pre-upgrade backup and a rollback record.
func preflightInstallVersionMove(ctx context.Context, cfg Config, deps Deps) error {
	if deps.RunHelm == nil || strings.TrimSpace(cfg.ChartPath) != "" {
		return nil
	}
	namespace := cfg.Namespace
	if namespace == "" {
		namespace = DefaultNamespace
	}
	release, exists, err := findHelmRelease(ctx, deps, "oberth", namespace)
	if err != nil || !exists {
		// The install itself reports an unreadable release list.
		return nil
	}
	installed := chartVersionFromRelease(release.Chart, "oberth")
	if planHelmAction(true, installed, cfg.ChartVersion, release.Status, cfg.Upgrade) != actionUpgrade {
		return nil
	}
	return refuseInstallVersionMove(installed, cfg.ChartVersion)
}

// refuseInstallVersionMove returns the refusal for moving an existing
// release from installed to target, or nil when both are the same version
// or either cannot be compared (the existing unknown-version handling
// applies then).
func refuseInstallVersionMove(installed, target string) error {
	i, t := canonicalChartVersion(installed), canonicalChartVersion(target)
	if i == "" || t == "" {
		return nil
	}
	switch cmp := semver.Compare(t, i); {
	case cmp < 0:
		return fmt.Errorf("installed Oberth %s is newer than this CLI (%s): refusing to downgrade the server, "+
			"whose newer database schema the older binary cannot open; upgrade the CLI first. "+
			"A deliberate rollback is oberth upgrade --rollback FILE (the record the upgrade wrote next to its backup)",
			displayVersion(installed), displayVersion(target))
	case cmp > 0:
		return fmt.Errorf("oberth %s is installed and this CLI is %s: version changes of an existing release go through "+
			"oberth upgrade (signed release verification, pre-upgrade backup, rollback record); "+
			"oberth install --upgrade reconciles configuration at the installed version only",
			displayVersion(installed), displayVersion(target))
	}
	return nil
}
