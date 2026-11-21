package releaseverify

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
)

// verifyBlob checks the pinned-key signature in a v0.3 message-signature bundle.
// Certificates and log material cannot substitute a different signing key.
func verifyBlob(payload, bundle, publicKey []byte) error {
	key, err := parseKey(publicKey)
	if err != nil {
		return err
	}
	var signed struct {
		MessageSignature struct {
			Signature []byte `json:"signature"`
		} `json:"messageSignature"`
	}
	if len(bundle) > MaxMetadata || json.Unmarshal(bundle, &signed) != nil {
		return Metadata
	}
	sum := sha256.Sum256(payload)
	message := signed.MessageSignature
	if !ecdsa.VerifyASN1(key, sum[:], message.Signature) {
		return Signature
	}
	return nil
}

func parseKey(publicKey []byte) (*ecdsa.PublicKey, error) {
	block, rest := pem.Decode(publicKey)
	if block == nil || block.Type != "PUBLIC KEY" || len(bytes.TrimSpace(rest)) != 0 {
		return nil, Key
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, Key
	}
	key, ok := parsed.(*ecdsa.PublicKey)
	if !ok || key.Curve != elliptic.P256() {
		return nil, Key
	}
	return key, nil
}

// CanonicalPublicKey validates a public-only P-256 PEM and normalizes its encoding.
// Provisioning uses the verifier's parser, so both accept the same key type.
func CanonicalPublicKey(publicKey []byte) ([]byte, error) {
	publicKey = bytes.TrimSpace(publicKey)
	if !bytes.HasPrefix(publicKey, []byte("-----BEGIN PUBLIC KEY-----")) {
		return nil, Key
	}
	key, err := parseKey(publicKey)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return nil, Key
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}), nil
}
