package installer

import "fmt"

// checkRelease uses only installation history and an explicit live/backup floor.
func (p *Plan) checkRelease() error {
	if err := p.request.CheckHistory(p.history); err != nil {
		return err
	}
	if p.minimumSchema == 0 {
		for _, revision := range []Revision{p.history.Latest, p.history.Deployed} {
			if revision.Number > 0 && revision.Version != p.release.Version() {
				return described("upgrade-required", nil, fmt.Sprintf("Revision %d: %s (%s).", revision.Number, revision.Version, revision.Status))
			}
		}
		return nil
	}
	if p.release.schema() < p.minimumSchema {
		return problem("schema-downgrade", nil)
	}
	return nil
}

// CheckHistory binds a request to the approved native deletion baseline and history.
func (r ChartRequest) CheckHistory(h History) error {
	if err := h.checkKind(); err != nil {
		return err
	}
	if err := r.checkBaseline(h); err != nil {
		return err
	}
	if r.Local && h.Verified {
		return problem("local-release", nil)
	}
	return nil
}

func (r ChartRequest) checkBaseline(h History) error {
	baseline := h.baseline()
	if h.Latest.Number != r.Revision {
		return described("target", nil, fmt.Sprintf("Latest revision changed after review: expected %d; found %d.", r.Revision, h.Latest.Number))
	}
	if baseline.Number != r.Predecessor {
		return described("target", nil, fmt.Sprintf("Predecessor changed after review: expected revision %d; found %d.", r.Predecessor, baseline.Number))
	}
	if baseline.ManifestDigest != r.ManifestDigest {
		return changedRevision(baseline.Number, nil)
	}
	return nil
}

func changedRevision(number int, cause error) error {
	return described("target", cause, fmt.Sprintf("Revision %d changed after review.", number))
}

func (r *ChartRequest) bindHistory(h History) {
	baseline := h.baseline()
	r.Revision, r.Predecessor, r.ManifestDigest = h.Latest.Number, baseline.Number, baseline.ManifestDigest
}

func (h History) baseline() Revision {
	if h.Deployed.Number > 0 {
		return h.Deployed
	}
	return h.Latest
}

func (h History) checkKind() error {
	if h.Verified && h.Latest.Kind == "local" {
		return problem("release-provenance", nil)
	}
	return nil
}

// Kind is recorded on the cluster release revision by the native action.
func (r ChartRequest) Kind() string {
	if r.Local {
		return "local"
	}
	return "verified"
}
