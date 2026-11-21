package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"

	"github.com/oberthci/oberth/internal/releaseverify"
	"github.com/oberthci/oberth/internal/staticgit"
	"github.com/oberthci/oberth/internal/workflowpolicy"
)

func target(settings []debug.BuildSetting, system, arch string) bool {
	values := make(map[string]string)
	for _, setting := range settings {
		values[setting.Key] = setting.Value
	}
	return values["GOOS"] == system && values["GOARCH"] == arch && values["CGO_ENABLED"] == "0"
}
func binaryNames() []string    { return releaseverify.PayloadNames()[:6] }
func publishedNames() []string { return releaseverify.PayloadNames() }
func packageRelease(ctx context.Context, o options) error {
	if !workflowpolicy.IsReleaseVersion(o.version) {
		return inputs
	}
	base, err := checkedBase(o)
	if err != nil {
		return err
	}
	hash, err := base.Digest()
	if err != nil {
		return err
	}
	images, err := images(o, base)
	if err != nil {
		return err
	}
	if err := chart(o, images); err != nil {
		return err
	}
	r := releaseverify.Record{Version: o.version, Commit: o.commit, Images: images, Base: hash.String(), Components: make(map[string]*buildinfo.BuildInfo), Git: make(map[string]staticgit.Record)}
	for _, arch := range []string{"amd64", "arm64"} {
		pin, err := o.git(staticgit.Architecture(arch))
		if err != nil {
			return err
		}
		r.Git[arch] = pin
	}
	if err := components(o, &r); err != nil {
		return err
	}
	return writeRecord(ctx, o, r)
}
func components(o options, r *releaseverify.Record) error {
	for _, name := range binaryNames() {
		info, err := buildinfo.ReadFile(filepath.Join(o.dir, name))
		if err != nil {
			return binariesMismatch
		}
		parts := strings.Split(name, "-")
		if !target(info.Settings, parts[len(parts)-2], parts[len(parts)-1]) {
			return binariesMismatch
		}
		r.Components[name] = info
	}
	return nil
}
func writeRecord(ctx context.Context, o options, r releaseverify.Record) error {
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if err := writeFile(o, "release.json", append(body, '\n')); err != nil {
		return err
	}
	names := publishedNames()
	slices.Sort(names)
	var sums strings.Builder
	for _, name := range names {
		hash, err := hashFile(ctx, filepath.Join(o.dir, name))
		if err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%s  %s\n", hash, name)
	}
	return writeFile(o, "SHA256SUMS", []byte(sums.String()))
}
func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = file.Close() })
	defer stop()
	sum := sha256.New()
	_, err = io.Copy(sum, file)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", sum.Sum(nil)), nil
}
