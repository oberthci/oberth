package model

import "slices"

// ScopeCovers reports whether an uplink scoped to these upstream names may
// see repositories of upstream (#073). No scopes cover everything: an
// unscoped uplink sees every repository.
func ScopeCovers(scopes []string, upstream string) bool {
	return len(scopes) == 0 || slices.Contains(scopes, upstream)
}
