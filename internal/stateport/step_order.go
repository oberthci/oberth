package stateport

import (
	"strconv"
	"strings"
)

func expandedStepLess(parent, a, b string) bool {
	left, leftOK := loopOrdinal(parent, a)
	right, rightOK := loopOrdinal(parent, b)
	if leftOK && rightOK && left != right {
		return left < right
	}
	return a < b
}
func loopOrdinal(parent, path string) (int, bool) {
	suffix, ok := strings.CutPrefix(path, parent+"(")
	if !ok {
		return 0, false
	}
	ordinal, _, ok := strings.Cut(suffix, ":")
	if !ok {
		return 0, false
	}
	n, err := strconv.Atoi(ordinal)
	return n, err == nil && n >= 0
}
