package model

import "slices"

// MetricsScope is the reserved uplink scope that may read GET /metrics and
// nothing else (#1534): no MCP tool, no REST view, no git, no admin. It is
// never an upstream name for scoping purposes, and it is always the
// uplink's only scope.
const MetricsScope = "metrics"

// MetricsOnly reports whether scopes is the metrics-only scope.
func MetricsOnly(scopes []string) bool {
	return len(scopes) == 1 && scopes[0] == MetricsScope
}

// ScopeCovers reports whether an uplink scoped to these upstream names may
// see repositories of upstream (#073). No scopes cover everything: an
// unscoped uplink sees every repository. The metrics-only scope covers none.
func ScopeCovers(scopes []string, upstream string) bool {
	if MetricsOnly(scopes) {
		return false
	}
	return len(scopes) == 0 || slices.Contains(scopes, upstream)
}
