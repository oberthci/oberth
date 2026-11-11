package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// defaultReleaseGetter fetches release objects when Deps names no getter.
var defaultReleaseGetter releaseverify.Getter = releaseverify.HTTPSGetter

// ReleaseSource names where a signed Oberth release is read from. Every
// field only changes the source; verification is the same for all of them.
type ReleaseSource struct {
	// Dir reads the release objects from a directory instead of fetching
	// them (air-gapped hosts).
	Dir string
	// Origin and ChartRepository override the publication origins (mirror).
	Origin          string
	ChartRepository string
}

// verifiedRelease is a signed Oberth release a command deploys: the chart
// bytes, written to a private file once their signature and the signed
// record's digest checked out, and the server image the record pins.
type verifiedRelease struct {
	chartPath string
	record    releaseverify.Record
	cleanup   func()
}

// verifyUpgradeRelease verifies the signed release of the CLI's own version.
func verifyUpgradeRelease(ctx context.Context, cfg UpgradeConfig, deps Deps) (*verifiedRelease, error) {
	return verifySignedRelease(ctx, cfg.BinaryVersion, ReleaseSource{Dir: cfg.ReleaseDir, Origin: cfg.ReleaseOrigin, ChartRepository: cfg.ChartRepository}, deps, "")
}

// verifySignedRelease fetches (or reads from the source directory) the
// release record, the chart and both signature bundles of version, and
// verifies them against the compiled-in release key. Nothing unverified
// reaches Helm: every failure refuses. The chart is written 0600 into dir,
// or into a fresh private temporary directory when dir is empty.
func verifySignedRelease(ctx context.Context, version string, src ReleaseSource, deps Deps, dir string) (*verifiedRelease, error) {
	tag := canonicalChartVersion(version)
	if tag == "" {
		return nil, fmt.Errorf("version %q is not a release version, so there is no signed release to verify (a development build deploys a local chart with --chart)", version)
	}
	var files releaseverify.Files
	var err error
	if src.Dir != "" {
		files, err = releaseverify.LoadDir(src.Dir, tag)
	} else {
		get := deps.ReleaseGetter
		if get == nil {
			get = defaultReleaseGetter
		}
		files, err = releaseverify.Fetch(ctx, get, src.Origin, src.ChartRepository, tag)
	}
	if err != nil {
		return nil, fmt.Errorf("signed release %s: %w", tag, err)
	}
	key := deps.releaseKey
	if key == nil {
		if key, err = releaseverify.PinnedKey(); err != nil {
			return nil, err
		}
	}
	release, err := releaseverify.Verify(files, tag, key)
	if err != nil {
		return nil, fmt.Errorf("refusing unverified release: %w", err)
	}
	cleanup := func() {}
	if dir == "" {
		if dir, err = os.MkdirTemp("", "oberth-release-*"); err != nil {
			return nil, fmt.Errorf("create private release directory: %w", err)
		}
		private := dir
		cleanup = func() { _ = os.RemoveAll(private) }
	}
	_, _, chartName, _ := releaseverify.Names(tag)
	path := filepath.Join(dir, chartName)
	if err := os.WriteFile(path, release.Chart, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write verified chart: %w", err)
	}
	return &verifiedRelease{chartPath: path, record: release.Record, cleanup: cleanup}, nil
}

// requireRecordImage refuses a chart whose default server image is not the
// one the signed record pins: the record, not the chart's default, is what
// the release pipeline attested.
func (r *verifiedRelease) requireRecordImage(chartImage string) error {
	if chartImage != r.record.Images.Server.Ref {
		return errors.New("the verified chart's default image.ref differs from the server image its signed release record pins")
	}
	return nil
}

func (r *verifiedRelease) describe(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Verified release %s: signed record (source %s@%s), chart %s, server image %s\n",
		r.record.Version, r.record.Source.Repository, r.record.Source.SHA[:12], r.record.Chart.Digest, r.record.Images.Server.Ref)
}

// UpgradeBackup is the pre-upgrade backup oberth upgrade took.
type UpgradeBackup struct {
	Path string // the encrypted archive
}

// verifyInstallRelease resolves the Oberth chart oberth install deploys.
// --chart is the development override and is not verified. Otherwise the
// signed release of cfg.ChartVersion is verified and its chart written to a
// private file, the chart's default server image must be the one the record
// pins, and an explicit --image must be that image too. It is idempotent on
// one cfg and one version; the returned cleanup removes the chart file. A
// cfg whose ChartVersion changed since it was verified (a reconcile at the
// installed version of a release newer than this CLI) is verified again for
// that version: the chart of another version must never reach the release.
func verifyInstallRelease(ctx context.Context, cfg *Config, deps Deps) (func(), error) {
	if strings.TrimSpace(cfg.ChartPath) != "" {
		return func() {}, nil
	}
	if cfg.verifiedChart != "" && cfg.verifiedVersion == canonicalChartVersion(cfg.ChartVersion) {
		return func() {}, nil
	}
	if deps.RunHelm == nil {
		deps.RunHelm = DefaultRunHelm
	}
	signed, err := verifySignedRelease(ctx, cfg.ChartVersion, cfg.Release, deps, "")
	if err != nil {
		return nil, err
	}
	chartImage, _, err := resolveChartImageRef(ctx, deps, signed.chartPath, "", true)
	if err == nil {
		err = signed.requireRecordImage(chartImage)
	}
	if err != nil {
		signed.cleanup()
		return nil, fmt.Errorf("verified chart %s: %w", signed.record.Version, err)
	}
	recordImage := signed.record.Images.Server.Ref
	if image := strings.TrimSpace(cfg.ImageRef); image != "" && image != recordImage {
		signed.cleanup()
		return nil, fmt.Errorf("--image %s is not the server image the signed release %s pins (%s); "+
			"an unverified image is a development install and needs --chart", image, signed.record.Version, recordImage)
	}
	if deps.Output != nil {
		signed.describe(deps.Output)
	}
	cfg.verifiedChart = signed.chartPath
	cfg.verifiedImage = recordImage
	cfg.verifiedVersion = canonicalChartVersion(cfg.ChartVersion)
	return signed.cleanup, nil
}

// oberthChart is the chart argument Helm gets for the Oberth release: the
// --chart override, else the verified release chart. Before verification
// (the dry-run plan) it names the chart the real run will verify.
func oberthChart(cfg Config) string {
	if local := strings.TrimSpace(cfg.ChartPath); local != "" {
		return local
	}
	if cfg.verifiedChart != "" {
		return cfg.verifiedChart
	}
	return "<verified oberth-" + strings.TrimPrefix(canonicalChartVersion(cfg.ChartVersion), "v") + ".tgz>"
}

// releaseSourceText says where a release is read from, for plans.
func releaseSourceText(src ReleaseSource) string {
	if src.Dir != "" {
		return "read from " + src.Dir
	}
	return "fetch from " + displayOr(src.Origin, releaseverify.DefaultReleaseOrigin) + " and " + displayOr(src.ChartRepository, releaseverify.DefaultChartRepository)
}
