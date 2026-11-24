package apicontract

import "github.com/oberthci/oberth/internal/publicerr"

// ExplicitUplinkScope requires a deliberate choice before granting repository access.
// Only the single wildcard spells an unrestricted grant.
func ExplicitUplinkScope(scope []string) ([]string, error) {
	if len(scope) == 1 && scope[0] == "*" {
		return []string{}, nil
	}
	if len(scope) == 0 {
		return nil, publicerr.New(publicerr.InvalidInput, "Choose an explicit uplink scope.", `Pass --scope '["origin"]' for an upstream or --scope '["*"]' for all repositories.`, nil)
	}
	return append([]string{}, scope...), nil
}
