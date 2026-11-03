//go:build !linux

package vmrunner

import (
	"context"
	"errors"
)

func readBeaconArtifact(context.Context, string) ([]byte, error) {
	return nil, errors.New("vmrunner: protected artifact capture is unavailable on this platform")
}
