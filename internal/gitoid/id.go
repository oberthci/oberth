package gitoid

// Valid recognizes full lowercase 40- or 64-character object identifiers.
// Formatting is not proof that an object exists; callers perform exact reads.
func Valid(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, c := range value {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
