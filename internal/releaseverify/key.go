package releaseverify

import (
	"bytes"
	_ "embed"
)

// The owner provisions this one file before the first release.
//
//go:embed release-cosign.pub
var pinnedKey []byte

// PublicKey returns the compiled owner key, refusing an unprovisioned pin.
func PublicKey() ([]byte, error) {
	if _, err := parseKey(pinnedKey); err != nil {
		return nil, err
	}
	return bytes.Clone(pinnedKey), nil
}
