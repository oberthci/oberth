package stateport

import (
	"path"
	"strings"
	"unicode"
)

// ValidRefGlob accepts shell wildcards over the whole Git ref, including '/'.
// Backslash escaping is omitted; public refs cannot contain backslashes.
func ValidRefGlob(pattern string) bool {
	if strings.ContainsRune(pattern, '\\') || strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
		return false
	}
	_, err := path.Match(strings.ReplaceAll(pattern, "/", "\x01"), "")
	return err == nil
}

// MatchRefGlob supplies the same whole-ref wildcard meaning to in-memory readers.
func MatchRefGlob(pattern, ref string) bool {
	if !ValidRefGlob(pattern) {
		return false
	}
	matched, _ := path.Match(strings.ReplaceAll(pattern, "/", "\x01"), strings.ReplaceAll(ref, "/", "\x01"))
	return matched
}
