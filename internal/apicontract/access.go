package apicontract

import (
	"github.com/oberthci/oberth/internal/actor"
	"reflect"
	"slices"
)

// Metrics is the authority class of the dedicated metrics endpoint.
const Metrics Access = "metrics"

// Valid reports the complete authority vocabulary shared with the service.
func (a Access) Valid() bool { return a == Read || a == ScopedAdmin || a == Admin || a == Metrics }

// Allows checks the class after authentication. Live identity, repository scope,
// policy-dependent actions and atomic audit remain the service's responsibility.
func (a Access) Allows(c actor.Actor) bool {
	metrics := slices.Contains(c.Scope, string(Metrics))
	if a == Metrics {
		return metrics || c.Admin && len(c.Scope) == 0
	}
	if metrics {
		return false
	}
	switch a {
	case Read:
		return true
	case ScopedAdmin:
		return c.Admin
	case Admin:
		return c.Admin && len(c.Scope) == 0
	}
	return false
}

// Action-only entries share tool authority declarations without adding tools.
type actionPolicies struct {
	Bindings
	Push    struct{} `tool:"push"`
	Backup  struct{} `tool:"backup" access:"unscoped_admin"`
	Metrics struct{} `tool:"metrics" access:"metrics"`
}

// Required is the service's action lookup. Unknown names have no authority.
// It reads the same declaration tags used by transport, including action-only
// push, backup and metrics. There is no service-owned shadow table.
func Required(name string) (Access, bool) {
	for _, f := range reflect.VisibleFields(reflect.TypeFor[actionPolicies]()) {
		if !f.Anonymous && f.Tag.Get("tool") == name {
			return declaredAccess(f.Tag), true
		}
	}
	return "", false
}
func declaredAccess(tag reflect.StructTag) Access {
	access := Access(tag.Get("access"))
	if access == "" {
		return Read
	}
	return access
}
