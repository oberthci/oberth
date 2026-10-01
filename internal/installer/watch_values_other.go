//go:build !linux

package installer

import (
	"errors"
	"os"
)

func sealedWatchValues([]byte) (*os.File, error) {
	return nil, errors.New("watch adoption requires Linux immutable public values input")
}
