package vmrunner

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
)

// CapturedBeaconArtifact owns its bytes in host memory. No candidate pathname
// is reopened after sealing, and callers cannot obtain its mutable byte slice.
// It is not a durable cache or evidence that a producer signature was verified.
type CapturedBeaconArtifact struct {
	body   []byte
	digest string
}

func CaptureBeaconArtifact(ctx context.Context, buildRoot string) (*CapturedBeaconArtifact, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	body, err := readBeaconArtifact(ctx, buildRoot)
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(body)
	return &CapturedBeaconArtifact{body: body, digest: "sha256:" + hex.EncodeToString(digest[:])}, nil
}

func (artifact *CapturedBeaconArtifact) Digest() string {
	if artifact == nil {
		return ""
	}
	return artifact.digest
}

func (artifact *CapturedBeaconArtifact) Size() int64 {
	if artifact == nil {
		return 0
	}
	return int64(len(artifact.body))
}

// Open checks the entire sealed plan before returning a read-only view. The
// wrapper intentionally exposes Read only, so WriterTo cannot lend our backing
// byte slice to an arbitrary destination.
func (artifact *CapturedBeaconArtifact) Open(plan PilotPlan) (io.ReadCloser, error) {
	if err := ValidatePilotPlan(plan); err != nil {
		return nil, err
	}
	if artifact == nil || artifact.Digest() != plan.ArtifactDigest || artifact.Size() != plan.ArtifactBytes {
		return nil, errors.New("vmrunner: captured artifact differs from sealed inputs")
	}
	return io.NopCloser(struct{ io.Reader }{bytes.NewReader(artifact.body)}), nil
}
