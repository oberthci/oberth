package installer

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/stateport"
	"helm.sh/helm/v3/pkg/chart"
	"helm.sh/helm/v3/pkg/chart/loader"
)

// Release owns the chart bytes authenticated by the canonical release verifier.
type Release struct {
	chart    *chart.Chart
	verified bool
}

func (r Release) Version() string { return r.chart.Metadata.AppVersion }
func (r Release) Verified() bool  { return r.verified }

// LoadRelease uses the same canonical files for online and offline sources.
// The only trust root is releaseverify's embedded owner pin.
func LoadRelease(ctx context.Context, files fs.FS, version string) (Release, error) {
	verified, err := releaseverify.Verify(ctx, files, version)
	if err != nil {
		refusal := releaseProblem(err)
		if source, ok := files.(interface{ ReleaseProblem(*Problem) }); ok {
			source.ReleaseProblem(refusal)
		}
		return Release{}, refusal
	}
	archive, err := verified.ReadFile("oberth.tgz", releaseverify.MaxChart)
	if err != nil {
		return Release{}, releaseProblem(err)
	}
	ch, err := loader.LoadArchive(bytes.NewReader(archive))
	if err != nil {
		return Release{}, releaseProblem(err)
	}
	record := verified.Record()
	if ch.Metadata.AppVersion != record.Version || ch.Metadata.Version != strings.TrimPrefix(record.Version, "v") ||
		ch.Values["imageDigest"] != record.Images["oberth"] || ch.Values["sourcePluginImageDigest"] != record.Images["oberth-source-plugin"] {
		return Release{}, problem("release", nil)
	}
	r := Release{chart: ch, verified: true}
	if r.schema() < 1 || r.protocol().Validate() != nil {
		return Release{}, problem("release", nil)
	}
	return r, nil
}

// LocalRelease is an explicit development override, always shown as unverified.
func LocalRelease(path string) (Release, error) {
	ch, err := loader.Load(path)
	if err != nil {
		return Release{}, problem("chart", err)
	}
	return Release{chart: ch}, nil
}

// Chart facts are authenticated by the archive, never extended release metadata.
type chartFacts struct {
	Schema int `json:"targetSchema"`
	Server struct {
		Protocol stateport.Protocol `json:"protocol"`
	} `json:"server"`
}

func (r Release) facts() chartFacts {
	var facts chartFacts
	raw, _ := json.Marshal(r.chart.Values)
	_ = json.Unmarshal(raw, &facts)
	return facts
}
func (r Release) schema() int                  { return r.facts().Schema }
func (r Release) protocol() stateport.Protocol { return r.facts().Server.Protocol }

func releaseProblem(err error) *Problem {
	p := NewProblem("release", err, "")
	if refusal, ok := err.(interface{ Remedy() string }); ok {
		p.Reason = err.Error()
		p.Remedy = refusal.Remedy()
		p.Detail = ""
	}
	return p
}
