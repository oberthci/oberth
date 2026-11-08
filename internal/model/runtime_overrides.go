package model

import (
	"encoding/json"
	"time"
)

// ReleaseWIFOverride is one administrator runtime decision for a canonical
// "upstream/org/repo" release WIF entry (#1497). Helm supplies the boot
// default; a granted override replaces that repository's entry and a revoked
// one removes it. Roles, namespace and dedicated step identities stay
// deployment configuration.
type ReleaseWIFOverride struct {
	Repo      string          `json:"repo"`
	Revoked   bool            `json:"revoked"`
	Grant     json.RawMessage `json:"grant,omitempty"`
	Reason    string          `json:"reason"`
	Actor     string          `json:"actor"`
	UpdatedAt time.Time       `json:"updated_at"`
}

// ReleaseWIFOverrideSpec is a validated grant or revoke request. A revoke
// carries no grant body.
type ReleaseWIFOverrideSpec struct {
	Repo    string
	Revoked bool
	Grant   json.RawMessage
	Reason  string
}

// AdmissionCapacityOverride is the server-owned runtime workflow ceiling. It
// takes precedence over the mounted file and the boot flag until cleared.
type AdmissionCapacityOverride struct {
	MaxConcurrentJobs int       `json:"max_concurrent_jobs"`
	Reason            string    `json:"reason"`
	Actor             string    `json:"actor"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// ReleaseWIFOverrideStatus reports a stored grant the server could not apply.
type ReleaseWIFOverrideStatus struct {
	Repo  string `json:"repo"`
	Error string `json:"error"`
}

// ReleaseWIFState is the public capability view: the boot default, the
// effective map after runtime overrides and any stored grant left inactive.
// It holds only public roles, ServiceAccount names and template mappings.
type ReleaseWIFState struct {
	Enabled   bool                       `json:"enabled"`
	Boot      map[string]json.RawMessage `json:"boot"`
	Effective map[string]json.RawMessage `json:"effective"`
	Roles     []string                   `json:"roles"`
	Inactive  []ReleaseWIFOverrideStatus `json:"inactive,omitempty"`
}
