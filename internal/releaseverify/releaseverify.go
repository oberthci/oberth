// Package releaseverify authenticates a published Oberth release before an
// operator command uses it: the signed release record (release.json), the
// Helm chart it pins by digest, and the server image digest it names.
//
// The release pipeline (.oberth/release.sh) signs each payload with
// `cosign sign-blob --key ... --bundle`, which writes a sigstore bundle whose
// messageSignature is an ECDSA P-256 signature over the payload's SHA-256.
// This package verifies exactly that, offline, against the release public key
// compiled into the binary (the same file as .oberth/pins/release-cosign.pub).
// Transparency-log entries in the bundle are not needed for that proof and
// are not consulted. Every failure refuses; nothing falls back to an
// unverified artifact.
package releaseverify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	_ "embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// ReleasePublicKeyPEM is the pinned release signing key.
//
//go:embed release-cosign.pub
var ReleasePublicKeyPEM []byte

// ReleasePublicKeySHA256 is the SHA-256 of ReleasePublicKeyPEM, the same pin
// release.sh checks (release_public_key_sha256).
const ReleasePublicKeySHA256 = "036dd71c9d3c07a19bff06d4392874014573f695620ac8554d9df175094c4a31"

// Size bounds for the payloads this package reads.
const (
	MaxRecordBytes = 64 << 10
	MaxBundleBytes = 1 << 20
	MaxChartBytes  = 16 << 20
)

// Component is the release record's component for the Oberth server.
const Component = "oberth"

// SourceRepository is the repository every Oberth release record names.
const SourceRepository = "github.com/oberthci/oberth"

var (
	sha256Digest  = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	gitSHA        = regexp.MustCompile(`^[0-9a-f]{40}$`)
	imageDigestOf = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]*(:[0-9]+)?(/[a-z0-9][a-z0-9._-]*)+(:[A-Za-z0-9_][A-Za-z0-9._-]{0,127})?@sha256:[0-9a-f]{64}$`)
)

// PublicKey parses a PEM ECDSA P-256 public key.
func PublicKey(pemBytes []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(pemBytes)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, errors.New("release public key is not one PEM PUBLIC KEY block")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse release public key: %w", err)
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, errors.New("release public key is not ECDSA P-256")
	}
	return key, nil
}

// PinnedKey returns the compiled-in release key after checking its pin.
func PinnedKey() (*ecdsa.PublicKey, error) {
	sum := sha256.Sum256(ReleasePublicKeyPEM)
	if hex.EncodeToString(sum[:]) != ReleasePublicKeySHA256 {
		return nil, errors.New("compiled-in release public key does not match its pin")
	}
	return PublicKey(ReleasePublicKeyPEM)
}

type bundle struct {
	MediaType            string `json:"mediaType"`
	VerificationMaterial struct {
		PublicKey *struct {
			Hint string `json:"hint"`
		} `json:"publicKey"`
	} `json:"verificationMaterial"`
	MessageSignature *struct {
		MessageDigest struct {
			Algorithm string `json:"algorithm"`
			Digest    string `json:"digest"`
		} `json:"messageDigest"`
		Signature string `json:"signature"`
	} `json:"messageSignature"`
}

// VerifyBlob checks that bundleJSON is a sigstore bundle whose message
// signature authenticates payload under key (cosign verify-blob --key).
func VerifyBlob(payload, bundleJSON []byte, key *ecdsa.PublicKey) error {
	if key == nil {
		return errors.New("no release public key")
	}
	if len(bundleJSON) == 0 || len(bundleJSON) > MaxBundleBytes {
		return errors.New("signature bundle is empty or too large")
	}
	var b bundle
	if err := json.Unmarshal(bundleJSON, &b); err != nil {
		return fmt.Errorf("decode signature bundle: %w", err)
	}
	if !strings.HasPrefix(b.MediaType, "application/vnd.dev.sigstore.bundle") {
		return fmt.Errorf("signature bundle media type %q is not a sigstore bundle", b.MediaType)
	}
	if b.MessageSignature == nil {
		return errors.New("signature bundle carries no message signature")
	}
	sum := sha256.Sum256(payload)
	if b.MessageSignature.MessageDigest.Digest != "" || b.MessageSignature.MessageDigest.Algorithm != "" {
		if b.MessageSignature.MessageDigest.Algorithm != "SHA2_256" {
			return fmt.Errorf("signature bundle digest algorithm %q is not SHA2_256", b.MessageSignature.MessageDigest.Algorithm)
		}
		digest, err := base64.StdEncoding.DecodeString(b.MessageSignature.MessageDigest.Digest)
		if err != nil || !bytes.Equal(digest, sum[:]) {
			return errors.New("signature bundle was made for different content")
		}
	}
	if b.VerificationMaterial.PublicKey != nil && b.VerificationMaterial.PublicKey.Hint != "" {
		der, err := x509.MarshalPKIXPublicKey(key)
		if err != nil {
			return err
		}
		hint := sha256.Sum256(der)
		if b.VerificationMaterial.PublicKey.Hint != base64.StdEncoding.EncodeToString(hint[:]) {
			return errors.New("signature bundle names a different signing key")
		}
	}
	signature, err := base64.StdEncoding.DecodeString(b.MessageSignature.Signature)
	if err != nil || len(signature) == 0 {
		return errors.New("signature bundle signature is not base64")
	}
	if !ecdsa.VerifyASN1(key, sum[:], signature) {
		return errors.New("signature does not verify under the pinned release key")
	}
	return nil
}

// Record is the subset of release.json an operator command relies on.
type Record struct {
	SchemaVersion int    `json:"schemaVersion"`
	Component     string `json:"component"`
	Version       string `json:"version"`
	Source        struct {
		Repository string `json:"repository"`
		SHA        string `json:"sha"`
	} `json:"source"`
	Images struct {
		Server struct {
			Ref string `json:"ref"`
		} `json:"server"`
	} `json:"images"`
	Chart struct {
		Version string `json:"version"`
		Digest  string `json:"digest"`
	} `json:"chart"`
}

// ParseRecord decodes and checks a release record for tag (vX.Y.Z).
func ParseRecord(raw []byte, tag string) (Record, error) {
	var record Record
	if len(raw) == 0 || len(raw) > MaxRecordBytes {
		return record, errors.New("release record is empty or too large")
	}
	if err := json.Unmarshal(raw, &record); err != nil {
		return record, fmt.Errorf("decode release record: %w", err)
	}
	switch {
	case record.SchemaVersion != 1:
		return record, fmt.Errorf("release record schemaVersion %d is not supported", record.SchemaVersion)
	case record.Component != Component:
		return record, fmt.Errorf("release record is for component %q, not %q", record.Component, Component)
	case record.Version != tag:
		return record, fmt.Errorf("release record is for %s, not %s", record.Version, tag)
	case record.Source.Repository != SourceRepository || !gitSHA.MatchString(record.Source.SHA):
		return record, errors.New("release record source is not an exact commit of the Oberth repository")
	case record.Chart.Version != strings.TrimPrefix(tag, "v"):
		return record, fmt.Errorf("release record chart version %q does not match %s", record.Chart.Version, tag)
	case !sha256Digest.MatchString(record.Chart.Digest):
		return record, errors.New("release record chart digest is not sha256:<64 hex>")
	case !imageDigestOf.MatchString(record.Images.Server.Ref):
		return record, errors.New("release record server image is not a digest-pinned reference")
	}
	return record, nil
}

// Release is a verified release: its record and the chart bytes the record
// pins.
type Release struct {
	Record Record
	Chart  []byte
}

// Files are the four published objects of one release.
type Files struct {
	Record, RecordBundle, Chart, ChartBundle []byte
}

// Verify authenticates the record and the chart, each by its own bundle, and
// checks the chart against the record's digest.
func Verify(files Files, tag string, key *ecdsa.PublicKey) (Release, error) {
	if err := VerifyBlob(files.Record, files.RecordBundle, key); err != nil {
		return Release{}, fmt.Errorf("release.json for %s: %w", tag, err)
	}
	record, err := ParseRecord(files.Record, tag)
	if err != nil {
		return Release{}, err
	}
	if len(files.Chart) == 0 || len(files.Chart) > MaxChartBytes {
		return Release{}, errors.New("release chart is empty or too large")
	}
	if err := VerifyBlob(files.Chart, files.ChartBundle, key); err != nil {
		return Release{}, fmt.Errorf("chart for %s: %w", tag, err)
	}
	sum := sha256.Sum256(files.Chart)
	if "sha256:"+hex.EncodeToString(sum[:]) != record.Chart.Digest {
		return Release{}, fmt.Errorf("chart for %s does not match the digest in its signed release record", tag)
	}
	return Release{Record: record, Chart: files.Chart}, nil
}

// Names returns the published object names of a release: the release record
// and its bundle (under <release origin>/oberth/<tag>/), and the chart and its
// bundle (under the chart repository).
func Names(tag string) (record, recordBundle, chart, chartBundle string) {
	chart = "oberth-" + strings.TrimPrefix(tag, "v") + ".tgz"
	return "release.json", "release.json.sigstore.json", chart, chart + ".sigstore.json"
}
