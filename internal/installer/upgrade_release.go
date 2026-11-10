package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/oberthci/oberth/internal/releaseverify"
)

// defaultReleaseGetter fetches release objects when Deps names no getter.
var defaultReleaseGetter releaseverify.Getter = releaseverify.HTTPSGetter

// verifiedUpgradeRelease is the signed release oberth upgrade deploys: the
// chart bytes, written to a private file once their signature and the signed
// record's digest checked out, and the server image the record pins.
type verifiedUpgradeRelease struct {
	chartPath string
	record    releaseverify.Record
	cleanup   func()
}

// verifyUpgradeRelease fetches (or reads from --release-dir) the release
// record, the chart and both signature bundles for the CLI's own version,
// and verifies them against the compiled-in release key. Nothing unverified
// reaches Helm: every failure refuses the upgrade.
func verifyUpgradeRelease(ctx context.Context, cfg UpgradeConfig, deps Deps) (*verifiedUpgradeRelease, error) {
	tag := canonicalChartVersion(cfg.BinaryVersion)
	if tag == "" {
		return nil, fmt.Errorf("target version %q is not a valid semantic version", cfg.BinaryVersion)
	}
	var files releaseverify.Files
	var err error
	if cfg.ReleaseDir != "" {
		files, err = releaseverify.LoadDir(cfg.ReleaseDir, tag)
	} else {
		get := deps.ReleaseGetter
		if get == nil {
			get = defaultReleaseGetter
		}
		files, err = releaseverify.Fetch(ctx, get, cfg.ReleaseOrigin, cfg.ChartRepository, tag)
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
	dir, err := os.MkdirTemp("", "oberth-release-*")
	if err != nil {
		return nil, fmt.Errorf("create private release directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	_, _, chartName, _ := releaseverify.Names(tag)
	path := filepath.Join(dir, chartName)
	if err := os.WriteFile(path, release.Chart, 0o600); err != nil {
		cleanup()
		return nil, fmt.Errorf("write verified chart: %w", err)
	}
	return &verifiedUpgradeRelease{chartPath: path, record: release.Record, cleanup: cleanup}, nil
}

// requireRecordImage refuses a chart whose default server image is not the
// one the signed record pins: the record, not the chart's default, is what
// the release pipeline attested.
func (r *verifiedUpgradeRelease) requireRecordImage(chartImage string) error {
	if chartImage != r.record.Images.Server.Ref {
		return errors.New("the verified chart's default image.ref differs from the server image its signed release record pins")
	}
	return nil
}

func (r *verifiedUpgradeRelease) describe(w io.Writer) {
	_, _ = fmt.Fprintf(w, "Verified release %s: signed record (source %s@%s), chart %s, server image %s\n",
		r.record.Version, r.record.Source.Repository, r.record.Source.SHA[:12], r.record.Chart.Digest, r.record.Images.Server.Ref)
}

// UpgradeBackup is the pre-upgrade backup oberth upgrade took.
type UpgradeBackup struct {
	Path string // the encrypted archive
}
