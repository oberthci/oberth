package main

import (
	"bytes"
	"os"

	"filippo.io/age"
	"filippo.io/age/armor"
)

const identityFileLimit int64 = 16 << 10

func readIdentity(path string) (*age.X25519Identity, error) {
	if err := regular(path, identityFileLimit, true); err != nil {
		return nil, err
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if bytes.HasPrefix(bytes.TrimSpace(body), []byte(armor.Header)) {
		return nil, recoveryError(identityProtected, nil)
	}
	if _, err := age.ExtractHeader(bytes.NewReader(body)); err == nil {
		return nil, recoveryError(identityProtected, nil)
	}
	identities, err := age.ParseIdentities(bytes.NewReader(body))
	if err != nil || len(identities) != 1 {
		return nil, recoveryError(identityInvalid, err)
	}
	identity, ok := identities[0].(*age.X25519Identity)
	if !ok {
		return nil, recoveryError(identityInvalid, nil)
	}
	return identity, nil
}
