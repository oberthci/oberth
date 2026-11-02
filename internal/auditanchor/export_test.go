package auditanchor

import (
	"crypto/ecdsa"
)

func newRekorWitness(api rekorAPI, verifier rekorEntryVerifier, keyMaterial []byte) (*RekorWitness, error) {
	return newRekorWitnessWithInfo(api, verifier, keyMaterial, DefaultWitnessKeyInfo)
}

func deriveWitnessKey(keyMaterial []byte) (*ecdsa.PrivateKey, error) {
	return deriveWitnessKeyWithInfo(keyMaterial, DefaultWitnessKeyInfo)
}
