package actor

import (
	"slices"
	"strings"
)

type Actor struct {
	AuthMethod  string   `json:"auth_method"`
	Identity    string   `json:"identity"`
	Fingerprint string   `json:"fingerprint"`
	Admin       bool     `json:"admin"`
	Scope       []string `json:"scope"`
}

// Clone copies the actor's scope independently of the authenticated value.
func (a Actor) Clone() Actor {
	a.Scope = slices.Clone(a.Scope)
	return a
}

// CanTouchRepository reports repository visibility for an authenticated actor.
// An identified actor with empty scope can touch every repository. Scope entries
// name upstreams. Admin does not bypass scope. Repository names are canonical
// upstream/org/name values.
func (a Actor) CanTouchRepository(repository string) bool {
	if a.Identity == "" || a.AuthMethod == "" {
		return false
	}
	upstream, _, ok := strings.Cut(repository, "/")
	if !ok || upstream == "" {
		return false
	}
	return len(a.Scope) == 0 || slices.Contains(a.Scope, upstream)
}
