package argojob

import (
	"path"
	"strings"
)

// New admission reserves the helper namespace. Recheck the actual main
// container too: a retained Pod can predate that admission rule.
func assistHelperShadow(value string) bool {
	if !path.IsAbs(value) || strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return true
	}
	value = path.Clean(value)
	for _, root := range []string{"/run/oberth", "/var/run/oberth"} {
		if value == root || strings.HasPrefix(value, root+"/") {
			return true
		}
	}
	return false
}
