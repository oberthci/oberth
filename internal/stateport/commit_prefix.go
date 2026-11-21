package stateport

import "encoding/hex"

// ValidCommitPrefix accepts a nonempty hexadecimal object selector.
func ValidCommitPrefix(prefix string) bool {
	if len(prefix) == 0 || len(prefix) > 64 {
		return false
	}
	// DecodeString validates pairs; an odd prefix is completed solely for validation.
	if len(prefix)%2 != 0 {
		prefix += "0"
	}
	_, err := hex.DecodeString(prefix)
	return err == nil
}
