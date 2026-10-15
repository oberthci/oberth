package installer

import (
	"errors"
	"strings"
)

// upgradeSetKey defines the deliberately narrower upgrade override grammar:
// one canonical dotted key per flag and a scalar string value. Helm's list,
// escaped-key and multi-assignment grammars are not accepted. Values retain
// Helm backslash escaping, including literal commas; never split escaped text.
func upgradeSetKey(assignment string) (string, error) {
	key, value, ok := strings.Cut(assignment, "=")
	refused := errors.New("upgrade --set requires one canonical dotted key and scalar value per flag; escape literal commas")
	if !ok || key == "" {
		return "", refused
	}
	for _, part := range strings.Split(key, ".") {
		if part == "" {
			return "", refused
		}
		for _, c := range part {
			if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '_' && c != '-' {
				return "", refused
			}
		}
	}
	escaped := false
	for _, c := range value {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == ',' || c == '{' || c == '}' {
			return "", refused
		}
	}
	if escaped {
		return "", refused
	}
	return key, nil
}
