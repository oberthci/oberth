package argojob

import (
	"path"
	"strings"

	"github.com/oberthci/oberth/internal/holdfs"
)

// New admission reserves the helper namespace. Recheck the actual main
// container too: a retained Pod can predate that admission rule.
func assistHelperShadow(value string) bool {
	if strings.Contains(value, "{{") || strings.Contains(value, "}}") {
		return true
	}
	value = path.Clean(value)
	for _, root := range []string{holdfs.HelperMountPath, "/var" + holdfs.HelperMountPath} {
		if value == "/" || value == root || strings.HasPrefix(value, root+"/") || strings.HasPrefix(root, value+"/") {
			return true
		}
	}
	return false
}
