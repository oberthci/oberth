package installer

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/release"
	"helm.sh/helm/v3/pkg/storage/driver"
)

func readHistory(ctx context.Context, config *action.Configuration, namespace string) (History, error) {
	if err := ctx.Err(); err != nil {
		return History{}, err
	}
	records, err := action.NewHistory(config).Run("oberth")
	if errors.Is(err, driver.ErrReleaseNotFound) {
		return History{}, nil
	}
	if err != nil {
		return History{}, err
	}
	var history History
	for _, record := range records {
		revision, err := readRevision(record, namespace)
		if err != nil {
			return History{}, err
		}
		history.add(revision)
	}
	if len(history.Foreign) > 0 && history.Choices.Number == 0 {
		return History{}, problem("release-unowned", nil)
	}
	slices.Sort(history.Foreign)
	return history, history.checkKind()
}
func (h *History) add(revision Revision) {
	h.Verified = h.Verified || revision.Kind == "verified"
	if revision.Kind == "foreign" {
		h.Foreign = append(h.Foreign, revision.Number)
	}
	if revision.Number > h.Latest.Number {
		h.Latest = revision
	}
	if revision.Status == "deployed" && revision.Number > h.Deployed.Number {
		h.Deployed = revision
	}
	if revision.applied() && revision.Number > h.Choices.Number {
		h.Choices = revision
	}
}

func (r Revision) applied() bool {
	return r.Installer() && (r.Status == "deployed" || r.Status == "superseded" || r.Status == "uninstalled")
}

func readRevision(r *release.Release, namespace string) (Revision, error) {
	if err := checkRevisionTarget(r, namespace); err != nil {
		return Revision{}, err
	}
	kind := r.Labels["oberth.ci/release-kind"]
	if r.Info == nil || r.Chart == nil || r.Chart.Metadata == nil || r.Version < 1 {
		return Revision{}, problem("release-provenance", nil)
	}
	digest := sha256.Sum256([]byte(r.Manifest))
	revision := Revision{Number: r.Version, Kind: kind, Version: r.Chart.Metadata.AppVersion,
		Status: string(r.Info.Status), ManifestDigest: base64.RawURLEncoding.EncodeToString(digest[:])}
	if (kind != "local" && kind != "verified") || r.Labels["oberth.ci/installer"] != "true" || r.Labels["oberth.ci/kind-revision"] != revisionBinding(r.Version, kind, r.Chart.Metadata.AppVersion) {
		revision.Kind = "foreign"
		return revision, nil
	}
	revision.Values, revision.RollbackID = r.Config, r.Labels["oberth.ci/rollback"]
	return revision, nil
}
func revisionLabels(r ChartRequest) map[string]string {
	return map[string]string{"oberth.ci/installer": "true", "oberth.ci/release-kind": r.Kind(), "oberth.ci/kind-revision": revisionBinding(r.Revision+1, r.Kind(), r.Chart.Metadata.AppVersion)}
}

// Bind source kind and application version to identify installer revisions.
// The unkeyed binding records provenance; it does not authenticate the writer.
func revisionBinding(number int, kind, version string) string {
	raw, _ := json.Marshal([]any{number, kind, version})
	digest := sha256.Sum256(raw)
	return "r-" + base64.RawURLEncoding.EncodeToString(digest[:])
}

// The native action may reread history after preflight. Reject a changed source
// before it creates a different revision; the native driver's Create is atomic.
type revisionWriteGuard struct {
	driver.Driver
	ctx     context.Context
	config  *action.Configuration
	request ChartRequest
}

func (g revisionWriteGuard) Create(key string, record *release.Release) error {
	if record.Version != g.request.Revision+1 {
		return described("target", nil, fmt.Sprintf("Expected to create revision %d; the native action selected %d.", g.request.Revision+1, record.Version))
	}
	if _, err := readRevision(record, g.request.Namespace); err != nil {
		return err
	}
	if err := checkApplyHistory(g.ctx, g.config, g.request); err != nil {
		return err
	}
	return g.Driver.Create(key, record)
}

func checkRevisionTarget(r *release.Release, namespace string) error {
	if r.Name != "oberth" || r.Namespace != namespace {
		return described("target", nil, fmt.Sprintf("Revision %d belongs to %s/%s, not %s/oberth.", r.Version, r.Namespace, r.Name, namespace))
	}
	if r.Chart != nil && r.Chart.Metadata != nil && r.Chart.Name() != "oberth" {
		return described("release-foreign", nil, "Helm release: "+namespace+"/oberth; chart: "+r.Chart.Name())
	}
	return nil
}
