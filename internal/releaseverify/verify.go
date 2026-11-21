// Package releaseverify authenticates release metadata and checks each consumed
// artifact while it is read, using the compiled owner key.
// Spec: docs/plans/c13-own-pipelines.md#owned-data-and-public-contract
package releaseverify

import (
	"bytes"
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"regexp"
	"strings"

	"github.com/oberthci/oberth/internal/staticgit"
)

// MaxMetadata bounds checksum manifests, bundles and release records.
const MaxMetadata = 1 << 20

// MaxArtifact bounds each binary. Chart archives have a separate 64 MiB limit.
const MaxArtifact = 512 << 20

// MaxChart bounds each authenticated chart archive.
const MaxChart = 64 << 20

// Record describes the release and its build inputs; SHA256SUMS owns file hashes.
type Record struct {
	Version    string                          `json:"version"`
	Commit     string                          `json:"commit"`
	Images     map[string]string               `json:"images"`
	Components map[string]*buildinfo.BuildInfo `json:"components"`
	Base       string                          `json:"base"`
	Git        map[string]staticgit.Record     `json:"git"`
}

// Refusal separates a short reason from the action the caller can take.
type Refusal string

const (
	Key       Refusal = "Release signing key is missing or invalid."
	Signature Refusal = "Release signature does not verify."
	Inventory Refusal = "Release files differ from the signed checksum list."
	Digest    Refusal = "Release file digest or length does not match."
	Metadata  Refusal = "Release metadata is invalid."
	Missing   Refusal = "Release metadata is missing."
	Limit     Refusal = "Release file exceeds its size limit."
	Read      Refusal = "Release file could not be read."
	Version   Refusal = "Release version differs from the requested version."
)

func (r Refusal) Error() string { return string(r) }
func (r Refusal) Remedy() string {
	return map[Refusal]string{
		Key:       "Build with the owner's public signing key at the canonical pin path.",
		Signature: "Fetch the owner's signed checksum manifest and matching signature.",
		Inventory: "Use only the files named in the signed checksum manifest.",
		Digest:    "Fetch the damaged artifact again before using it.",
		Metadata:  "Fetch a release with a valid release record and checksum manifest.",
		Missing:   "Fetch the checksum manifest, its signature and release.json.",
		Limit:     "Use release files within the documented size limits.",
		Read:      "Check the release file's permissions and storage, then retry.",
		Version:   "Request the exact version recorded in the signed release.",
	}[r]
}

// Release holds authenticated metadata and opens only signed names.
type Release struct {
	ctx          context.Context
	files        fs.FS
	listed       map[string]string
	record       Record
	body         []byte
	sums, bundle []byte
}

// Verify authenticates the checksum list before reading release.json once.
// Only the compiled public key supplies signing authority.
func Verify(ctx context.Context, files fs.FS, version string) (*Release, error) {
	key, err := PublicKey()
	if err != nil {
		return nil, Key
	}
	return verify(ctx, files, key, version)
}
func verify(ctx context.Context, files fs.FS, key []byte, version string) (*Release, error) {
	sums, bundle, err := verifiedManifest(ctx, files, key)
	if err != nil {
		return nil, err
	}
	listed, err := parseSums(sums)
	if err != nil {
		return nil, err
	}
	r := &Release{ctx: ctx, files: files, listed: listed, sums: sums, bundle: bundle}
	body, err := r.ReadFile("release.json", MaxMetadata)
	if err != nil {
		return nil, err
	}
	r.record, err = parseRecord(body, version)
	if err != nil {
		return nil, err
	}
	r.body = body
	return r, nil
}

// Record returns the record decoded from the exact authenticated bytes.
func (r *Release) Record() Record { return r.record }

// Manifest returns copies of the authenticated checksum list and its bundle.
// Publication preserves these exact bytes alongside the checked artifacts.
func (r *Release) Manifest() ([]byte, []byte) { return bytes.Clone(r.sums), bytes.Clone(r.bundle) }

func parseRecord(body []byte, version string) (Record, error) {
	var record Record
	if err := strictRecord(body, &record); err != nil {
		return record, Metadata
	}
	if version == "" || record.Version != version {
		return record, Version
	}
	if len(record.Commit) != 40 || !validHex(record.Commit) {
		return record, Metadata
	}
	if len(record.Images) != 2 || !imageDigest(record.Images["oberth"]) || !imageDigest(record.Images["oberth-source-plugin"]) || !imageDigest(record.Base) {
		return record, Metadata
	}
	return record, nil
}
func imageDigest(s string) bool {
	return len(s) == 71 && strings.HasPrefix(s, "sha256:") && validHex(s[7:])
}
func validHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil && strings.ToLower(s) == s
}
func parseSums(body []byte) (map[string]string, error) {
	listed := make(map[string]string)
	previous := ""
	namePattern := regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		digest, name, ok := strings.Cut(line, "  ")
		if !ok || len(digest) != 64 || !validHex(digest) || !namePattern.MatchString(name) || name <= previous || strings.HasPrefix(name, "SHA256SUMS") {
			return nil, Inventory
		}
		listed[name], previous = digest, name
	}
	if listed["release.json"] == "" {
		return nil, Inventory
	}
	return listed, nil
}
func readMetadata(ctx context.Context, files fs.FS, name string) ([]byte, error) {
	f, err := openRegular(files, name, MaxMetadata)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, Missing
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(io.LimitReader(&contextReader{ctx: ctx, reader: f}, MaxMetadata+1))
	if err != nil {
		return nil, err
	}
	if len(body) > MaxMetadata {
		return nil, Limit
	}
	return body, nil
}

// CheckFiles consumes the complete signed payload set, allowing the two local
// image-layout directories produced by packaging; installers may read a subset.
func (r *Release) CheckFiles() error {
	entries, err := localPayloadFiles(r.files)
	if err != nil {
		return err
	}
	if len(entries) != 10 {
		return Inventory
	}
	if err := r.CheckInventory(); err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.Type().IsRegular() {
			return Inventory
		}
		name := entry.Name()
		if name == "SHA256SUMS" || name == "SHA256SUMS.sigstore.json" {
			continue
		}
		f, err := r.Open(name)
		if err != nil {
			return err
		}
		if err := f.Close(); err != nil {
			return err
		}
	}
	return nil
}

// ReadFile returns exactly the checked bytes within the caller's smaller bound.
func (r *Release) ReadFile(name string, max int64) ([]byte, error) {
	f, err := r.Open(name)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(io.LimitReader(f, max+1))
	closeErr := f.Close()
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > max {
		return nil, Limit
	}
	return body, closeErr
}

// Open returns checked bytes. Callers stage them privately, then install only
// after a successful final read and Close. The final bytes are withheld on a
// digest mismatch. Close drains unread bytes and verifies them.
func (r *Release) Open(name string) (io.ReadCloser, error) {
	want, ok := r.listed[name]
	if !ok {
		return nil, Inventory
	}
	if name == "release.json" && r.body != nil {
		return io.NopCloser(bytes.NewReader(r.body)), nil
	}
	max := int64(MaxArtifact)
	if name == "release.json" {
		max = MaxMetadata
	}
	if name == "oberth.tgz" {
		max = MaxChart
	}
	f, err := openRegular(r.files, name, max)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return &checkedReader{file: f, ctx: r.ctx, sum: sha256.New(), want: want, length: info.Size(), max: max}, nil
}

func openRegular(files fs.FS, name string, max int64) (fs.File, error) {
	info, err := fs.Lstat(files, name)
	if err != nil {
		return nil, readFailure(err)
	}
	if !info.Mode().IsRegular() {
		return nil, Inventory
	}
	if info.Size() > max {
		return nil, Limit
	}
	f, err := files.Open(name)
	if err != nil {
		return nil, readFailure(err)
	}
	info, err = f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = Inventory
	}
	if err == nil && (info.Size() > max) {
		err = Limit
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// PayloadNames is the single exact payload inventory used by release CI.
func PayloadNames() []string {
	return []string{"oberth-darwin-amd64", "oberth-darwin-arm64", "oberth-linux-amd64", "oberth-linux-arm64", "oberth-source-plugin-linux-amd64", "oberth-source-plugin-linux-arm64", "oberth.tgz", "release.json"}
}

type readError struct{ cause error }

func (e readError) Detail() string { return e.cause.Error() }

func (e readError) Error() string   { return Read.Error() }
func (e readError) Remedy() string  { return Read.Remedy() }
func (e readError) Unwrap() []error { return []error{Read, e.cause} }
func readFailure(err error) error   { return readError{cause: err} }

// CheckInventory requires the eight public payloads for publication or clean CI.
// Consumer verification remains selective; local image layouts are push inputs.
func (r *Release) CheckInventory() error {
	if len(r.listed) != 8 {
		return Inventory
	}
	for _, name := range PayloadNames() {
		if r.listed[name] == "" {
			return Inventory
		}
	}
	return nil
}

func localPayloadFiles(files fs.FS) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil, err
	}
	payloads := make([]fs.DirEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && (entry.Name() == "oberth.oci" || entry.Name() == "oberth-source-plugin.oci") {
			continue
		}
		payloads = append(payloads, entry)
	}
	return payloads, nil
}
