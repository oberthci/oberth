package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/workflowpolicy"
	"golang.org/x/mod/semver"
)

type verification struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	Sums    string `json:"sums"`
}

func writeVerification(ctx context.Context, o options, record releaseverify.Record) error {
	sums, err := hashFile(ctx, filepath.Join(o.dir, "SHA256SUMS"))
	if err != nil {
		return err
	}
	body, err := json.Marshal(verification{Version: record.Version, Commit: record.Commit, Sums: sums})
	if err != nil {
		return err
	}
	return os.WriteFile(o.receipt, body, 0600)
}
func verified(ctx context.Context, o options, record releaseverify.Record) error {
	body, err := readSmallFile(o.receipt)
	if err != nil {
		return notVerified
	}
	var proof verification
	sums, err := hashFile(ctx, filepath.Join(o.dir, "SHA256SUMS"))
	if err != nil || json.Unmarshal(body, &proof) != nil || proof.Version != record.Version || proof.Commit != record.Commit || proof.Sums != sums {
		return notVerified
	}
	return nil
}
func publish(ctx context.Context, o options) error {
	p, err := publicationFor(o)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(o.dir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	release, err := releaseverify.Verify(ctx, root.FS(), o.version)
	if err != nil {
		return err
	}
	record := release.Record()
	if err := checkPublishedImages(ctx, release, o.dir); err != nil {
		return err
	}
	staged, err := os.MkdirTemp("", "publication-")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staged) }()
	if err := stagePublication(release, staged); err != nil {
		return err
	}
	if !workflowpolicy.IsReleaseVersion(record.Version) {
		return inputs
	}
	p.dir = staged
	if err := p.publishFiles(ctx, record.Version, record.Commit); err != nil {
		return err
	}
	_, err = fmt.Printf("Published %s: %d files at %s/%s.\n", record.Version, len(publishedNames())+2, releaseverify.Origin, record.Version)
	return err
}
func finalize(ctx context.Context, o options) error {
	p, err := publicationFor(o)
	if err != nil {
		return err
	}
	record, err := loadVerified(ctx, o)
	if err != nil {
		return err
	}
	if err := verified(ctx, o, record); err != nil {
		return err
	}
	return p.finish(ctx, record.Version, os.Stdout)
}

// publishFiles makes the tag's GitHub release hold exactly these files. It
// compares every asset already there before uploading anything, uploads the
// manifest last, confirms the draft, and publishes it without making it latest.
func (p publication) publishFiles(ctx context.Context, tag, commit string) error {
	if err := p.tagPublished(ctx, tag, commit); err != nil {
		return err
	}
	release, err := p.releaseFor(ctx, tag)
	if err != nil {
		return err
	}
	names, err := p.names()
	if err != nil {
		return err
	}
	if err := p.addMissing(ctx, release, names); err != nil {
		return err
	}
	if !release.Draft {
		return nil
	}
	if err := p.confirmDraft(ctx, release.ID, names); err != nil {
		return err
	}
	return p.publishDraft(ctx, release.ID)
}

// addMissing compares every asset the release holds before uploading the
// missing names in order; published bytes are never replaced.
func (p publication) addMissing(ctx context.Context, release githubRelease, names []string) error {
	existing := make(map[string]githubAsset, len(release.Assets))
	for _, asset := range release.Assets {
		existing[asset.Name] = asset
	}
	for _, name := range names {
		if asset, ok := existing[name]; ok {
			if err := p.sameAsset(ctx, asset); err != nil {
				return err
			}
		}
	}
	for _, name := range names {
		if _, ok := existing[name]; !ok {
			if err := p.upload(ctx, release.ID, name); err != nil {
				return err
			}
		}
	}
	return nil
}

// confirmDraft re-reads the draft and requires exactly the signed names, each
// equal to its local bytes, before the release becomes public.
func (p publication) confirmDraft(ctx context.Context, id int64, names []string) error {
	var release githubRelease
	if err := p.expect(ctx, get(p.repo("/releases/%d", id)), http.StatusOK, &release); err != nil {
		return err
	}
	held := make([]string, 0, len(release.Assets))
	for _, asset := range release.Assets {
		held = append(held, asset.Name)
	}
	slices.Sort(held)
	if want := slices.Sorted(slices.Values(names)); !slices.Equal(held, want) {
		return fmt.Errorf("%w: the draft holds %s", assetsDiffer, strings.Join(held, ", "))
	}
	for _, asset := range release.Assets {
		if err := p.sameAsset(ctx, asset); err != nil {
			return err
		}
	}
	return nil
}

func (p publication) names() ([]string, error) {
	entries, err := os.ReadDir(p.dir)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), ".oci") && entry.IsDir() {
			continue
		}
		if !entry.Type().IsRegular() {
			return nil, inputs
		}
		names = append(names, entry.Name())
	}
	slices.SortFunc(names, func(a, b string) int { return publicationRank(a) - publicationRank(b) })
	return names, nil
}

// sameAsset accepts an existing asset only when GitHub's digest and size equal
// the local file: published bytes are never replaced.
func (p publication) sameAsset(ctx context.Context, asset githubAsset) error {
	if asset.State != "uploaded" || !strings.HasPrefix(asset.Digest, "sha256:") {
		return fmt.Errorf("%w: asset %s is %s with digest %q", assetUnconfirmed, asset.Name, asset.State, asset.Digest)
	}
	path := filepath.Join(p.dir, asset.Name)
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	digest, err := hashFile(ctx, path)
	if err != nil {
		return err
	}
	if asset.Size != info.Size() || asset.Digest != "sha256:"+digest {
		return fmt.Errorf("%w: asset %s", immutable, asset.Name)
	}
	return nil
}

// finish marks the verified tag as GitHub's latest release only when it is
// newer than the current latest, so latest never moves backwards.
func (p publication) finish(ctx context.Context, tag string, output io.Writer) error {
	current, err := p.latestTag(ctx)
	if err != nil {
		return err
	}
	page := "https://github.com/" + p.repository + "/releases/latest"
	switch {
	case current == tag:
		_, err = fmt.Fprintf(output, "Latest at %s is %s.\n", page, tag)
		return err
	case current != "" && semver.Compare(current, tag) > 0:
		_, err = fmt.Fprintf(output, "Latest at %s stays %s; %s was not made latest.\n", page, current, tag)
		return err
	}
	if err := p.makeLatest(ctx, tag); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "Latest at %s is %s.\n", page, tag)
	return err
}

func readSmallFile(path string) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	body, err := io.ReadAll(io.LimitReader(file, releaseverify.MaxMetadata+1))
	if err != nil {
		return nil, err
	}
	if len(body) > releaseverify.MaxMetadata {
		return nil, releaseverify.Limit
	}
	return body, nil
}

func publicationRank(name string) int {
	switch name {
	case "release.json":
		return 1
	case "SHA256SUMS":
		return 2
	case "SHA256SUMS.sigstore.json":
		return 3
	}
	return 0
}
