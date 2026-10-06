// Package identityname provides the canonical name derivation functions for
// per-repo and per-step Vault identities. Both internal/installer (which
// materializes policies, roles, and ServiceAccounts) and internal/service
// (which computes the plan and its digest) import this package so the names
// they produce are guaranteed identical. A divergence here is a sync/plan
// mismatch that shows "stale" even when the materialized state is correct.
//
// Issue #623, finding 3: the plan used a simple "oberth-step-<repo>-<step>"
// derivation while the installer used PerStepName with a hash suffix. This
// package is the single source of truth.
package identityname

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// MaxNameLength caps generated names so they fit within the Kubernetes label
// value limit (63 characters). Shared across per-repo and per-step derivations.
const MaxNameLength = 63

// Per-repo prefixes.
const (
	PerRepoPrefix   = "oberth-argo-"
	PerRepoCIPrefix = "oberth-argo-ci-"
)

// Per-step prefix.
const PerStepPrefix = "oberth-step-"

// PerStepName generates the deterministic, DNS-1123-safe name for a per-step
// identity. The name is shared across the ServiceAccount, the Vault policy,
// and the Vault role, because they are a single identity.
//
// The hash input is the full canonical quad (upstream/org/repo/step) so two
// steps in different repos that sanitise to the same readable portion get
// different names.
func PerStepName(upstream, org, repo, step string) string {
	canonical := upstream + "/" + org + "/" + repo + "/" + step
	digest := sha256.Sum256([]byte(canonical))
	hashSuffix := hex.EncodeToString(digest[:])[:12]

	var safe strings.Builder
	for _, c := range strings.ToLower(upstream + "-" + org + "-" + repo + "-" + step) {
		switch {
		case c >= 'a' && c <= 'z',
			c >= '0' && c <= '9',
			c == '-':
			safe.WriteRune(c)
		default:
			safe.WriteByte('-')
		}
	}
	readable := strings.Trim(safe.String(), "-")

	// Budget: prefix(12) + readable + "-" + hash(12) = total ≤ 63
	maxReadable := MaxNameLength - len(PerStepPrefix) - 1 - len(hashSuffix)
	if len(readable) > maxReadable {
		readable = strings.TrimRight(readable[:maxReadable], "-")
	}
	if readable == "" {
		readable = "step"
	}
	return PerStepPrefix + readable + "-" + hashSuffix
}

// PerStepNameFromQualified derives the per-step name from a qualified repo
// string ("upstream/org/repo") and a step name. It splits the qualified repo
// and delegates to PerStepName. Returns empty string if the repo is not in
// the expected 3-segment format.
func PerStepNameFromQualified(qualifiedRepo, step string) string {
	parts := strings.SplitN(qualifiedRepo, "/", 3)
	if len(parts) != 3 {
		return ""
	}
	return PerStepName(parts[0], parts[1], parts[2], step)
}

// HasPerStepPrefix reports whether name begins with the per-step prefix.
// Used by orphan detection to identify per-step policies/roles that may
// need removal when their grant is revoked.
func HasPerStepPrefix(name string) bool {
	return strings.HasPrefix(name, PerStepPrefix)
}
