// Package verifytest supplies signing fixtures and compiled pins for consumer tests.
// Spec: docs/plans/c13-own-pipelines.md#owned-data-and-public-contract
package verifytest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
)

// This public, synthetic key is only a fixture, never a product trust root.
func fixtureKey() (*ecdsa.PrivateKey, error) {
	scalar := make([]byte, 32)
	scalar[31] = 1
	return ecdsa.ParseRawPrivateKey(elliptic.P256(), scalar)
}

// Sign returns the native Sigstore bundle for the supplied checksum manifest.
func Sign(body []byte) ([]byte, error) {
	sum := sha256.Sum256(body)
	key, err := fixtureKey()
	if err != nil {
		return nil, err
	}
	signature, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"mediaType": "application/vnd.dev.sigstore.bundle.v0.3+json", "messageSignature": map[string]any{"messageDigest": map[string]any{"algorithm": "SHA2_256", "digest": sum[:]}, "signature": signature}})
}
