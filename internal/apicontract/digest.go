package apicontract

import (
	"crypto/sha256"
	"encoding/hex"
)

// InvitationSecretMinChars is the service-owned minimum invitation secret length in characters.
const InvitationSecretMinChars = 32

// SecretDigest is the wire digest of an opaque bearer or invitation secret.
// Producers retain plaintext locally; services persist only this representation.
func SecretDigest(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}
