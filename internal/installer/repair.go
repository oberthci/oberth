package installer

import (
	"context"
	"fmt"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
)

// Recovery binds the exact unfinished revision approved for repair.
type Recovery struct {
	Pending, Deployed int
}

func recoveryFor(h History) (*Recovery, error) {
	if !unfinished(h.Latest.Status) {
		if h.Latest.Number == 0 || h.Latest.Status == "deployed" || h.Latest.Status == "failed" || (h.Latest.Status == "superseded" && h.Deployed.Number == 0) {
			return nil, nil
		}
		return nil, described("chart-state", nil, fmt.Sprintf("Latest revision %d: %s.", h.Latest.Number, h.Latest.Status))
	}
	if !h.Latest.Installer() && h.Latest.Status != "uninstalled" {
		if h.Latest.Status == "uninstalling" {
			return nil, problem("foreign-removed", nil)
		}
		return nil, problem("pending", nil)
	}
	return &Recovery{Pending: h.Latest.Number, Deployed: h.Deployed.Number}, nil
}
func (l ChartLibrary) repair(ctx context.Context, config *action.Configuration, r ChartRequest) error {
	if err := checkApplyHistory(ctx, config, r); err != nil {
		return err
	}
	// Validate fresh target bytes before changing history bookkeeping.
	if _, err := Render(r.Chart, r.Values, r.Namespace); err != nil {
		return err
	}
	last, err := config.Releases.Last("oberth")
	if err != nil {
		return err
	}
	revision, err := readRevision(last, r.Namespace)
	if err != nil {
		return err
	}
	if revision.Number != r.Recovery.Pending || (!revision.Installer() && revision.Status != "uninstalled") || !unfinished(revision.Status) {
		return changedRevision(last.Version, nil)
	}
	if err := r.Recovery.checkDeployed(config); err != nil {
		return err
	}
	last.Info.Status = repairStatus(last.Info.Status)
	return config.Releases.Update(last)
}

// Removed revisions retain their reviewed choices while the native upgrade retries.
func repairStatus(status release.Status) release.Status {
	if status == release.StatusUninstalled {
		return release.StatusSuperseded
	}
	return release.StatusFailed
}

func (r Recovery) checkDeployed(config *action.Configuration) error {
	if r.Deployed == 0 {
		return nil
	}
	target, err := config.Releases.Get("oberth", r.Deployed)
	if err != nil || target.Info.Status != release.StatusDeployed {
		return changedRevision(r.Deployed, err)
	}
	return nil
}

// unfinished includes removals whose release history has not yet been purged.
func unfinished(status string) bool {
	return status == "pending-install" || status == "pending-upgrade" || status == "pending-rollback" || status == "uninstalling" || status == "uninstalled"
}

// A rollback uses fresh authenticated bytes; only the exact owned pending revision is unlocked.
func (l ChartLibrary) rollbackRevision(config *action.Configuration, r ChartRequest, dry bool) error {
	if _, err := Render(r.Chart, r.Values, r.Namespace); err != nil {
		return err
	}
	last, err := config.Releases.Last("oberth")
	if err != nil {
		return err
	}
	revision, err := readRevision(last, r.Namespace)
	if err != nil {
		return err
	}
	if revision.Number != r.Revision || !revision.Installer() {
		return changedRevision(last.Version, nil)
	}
	if !dry && last.Info.Status.IsPending() {
		last.Info.Status = release.StatusFailed
		return config.Releases.Update(last)
	}
	return nil
}

func (l ChartLibrary) repairRequest(ctx context.Context, config *action.Configuration, r ChartRequest) (ChartRequest, error) {
	if r.Recovery == nil {
		return r, nil
	}
	if err := l.repair(ctx, config, r); err != nil {
		return r, err
	}
	r.Revision = r.Recovery.Pending
	return r, nil
}
