package stateport

import "encoding/json"

// IdentityRegistration binds all public identity material in one audited commit.
type IdentityRegistration struct {
	Key        Key
	Credential *Credential
	SPKI       string
}
type KeyReplacement struct{ Previous, Replacement string }
type Upstream struct {
	Name, Org, Kind, BaseURL, KeyName string
	Removed                           bool
}

// GrantKind is part of an exact authority tuple, never inferred from its path.
type GrantKind string

const (
	GrantStorePath  GrantKind = "store_path"
	GrantSecret     GrantKind = "secret"
	GrantConfigMap  GrantKind = "configmap"
	GrantImagePull  GrantKind = "image_pull_secret"
	GrantPVC        GrantKind = "pvc"
	GrantDevice     GrantKind = "device"
	GrantAudience   GrantKind = "identity"
	GrantCapability GrantKind = "capability"
)

func (k GrantKind) Valid() bool {
	switch k {
	case GrantStorePath, GrantSecret, GrantConfigMap, GrantImagePull, GrantPVC, GrantDevice, GrantAudience, GrantCapability:
		return true
	}
	return false
}

type GrantChange struct {
	Kind                               GrantKind
	Repository, File, Step, Path, Tier string
	Capability                         json.RawMessage
	Revoke, Dismiss                    bool
}

// AuthoritySnapshot contains one committed generation. Callers publish the whole
// value; they never merge separate reads into a wider effective authority view.
type AuthoritySnapshot struct {
	Revision int64
	Grants   []GrantChange
}

func (g GrantChange) Clone() GrantChange {
	g.Capability = append(json.RawMessage(nil), g.Capability...)
	return g
}
func (v AuthoritySnapshot) Clone() AuthoritySnapshot {
	out := AuthoritySnapshot{Revision: v.Revision, Grants: make([]GrantChange, 0, len(v.Grants))}
	for _, g := range v.Grants {
		out.Grants = append(out.Grants, g.Clone())
	}
	return out
}

// GrantDismissalRefusal describes the pending-only authority mutation.
func GrantDismissalRefusal() (string, string) {
	return "Only pending requests can be dismissed.", "Use oberth access revoke for an approved or revoked grant."
}
