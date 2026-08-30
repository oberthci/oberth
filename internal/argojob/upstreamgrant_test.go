package argojob

import (
	"strings"
	"testing"

	"github.com/oberthci/oberth/pkg/periapsis"
)

// grantRequest is the minimum authorizeWithApprovalTable reads.
func grantRequest(trigger periapsis.Trigger, approved map[string]bool) Request {
	if approved == nil {
		approved = map[string]bool{}
	}
	return Request{
		Repo: "oberth", UpstreamOrg: "skipops",
		Trigger:         trigger,
		ApprovedSecrets: approved,
	}
}

// TestUpstreamScopedPathsNeedNoGrant is the change: a path in the
// oberth/upstream/ namespace is authorized structurally against the pushing
// repository's own upstream org and catalog name, so an approval-table row can
// only restate a constraint that is already enforced. This is what
// AGENT-CONTRACT.md documents and what argoworkflow.AuthorizeSecretPaths, the
// same gate for the other authoring format, has always done.
func TestUpstreamScopedPathsNeedNoGrant(t *testing.T) {
	t.Parallel()
	for _, declared := range []string{
		"oberth/upstream/skipops/github-token",       // the whole org's
		"oberth/upstream/skipops/oberth/credentials", // this repository's own
	} {
		for _, trigger := range []periapsis.Trigger{periapsis.TriggerCI, periapsis.TriggerRelease} {
			// Empty approval table on purpose: no administrator ran anything.
			err := authorizeWithApprovalTable([]string{declared}, grantRequest(trigger, nil))
			if err != nil {
				t.Fatalf("%s under %s should be admitted without a grant: %v", declared, trigger, err)
			}
		}
	}
}

// TestUpstreamScopingStillFencesOtherTenants proves the structural check is
// what is doing the work, not the absence of a check. A repository must not
// reach another org's subtree or a sibling repository's, grant or no grant.
func TestUpstreamScopingStillFencesOtherTenants(t *testing.T) {
	t.Parallel()
	for _, declared := range []string{
		"oberth/upstream/othercorp/github-token",
		"oberth/upstream/othercorp/oberth/credentials",
		"oberth/upstream/skipops/some-other-repo/credentials",
	} {
		// Granted, to show the grant is not what admits it either way.
		request := grantRequest(periapsis.TriggerRelease, map[string]bool{declared: true})
		if err := authorizeWithApprovalTable([]string{declared}, request); err == nil {
			t.Fatalf("%s must be refused: it is outside this repository's own subtree", declared)
		}
	}
}

// TestSystemPathsStillRequireAGrant is the other half of the condition: the
// gate must provably stay intact for everything outside the hierarchical
// namespace. A release credential belongs to the deployment, and nothing about
// the pushing repository implies it may read one.
func TestSystemPathsStillRequireAGrant(t *testing.T) {
	t.Parallel()
	const declared = "oberth/data/release/cosign-secret"

	err := authorizeWithApprovalTable([]string{declared}, grantRequest(periapsis.TriggerRelease, nil))
	if err == nil {
		t.Fatal("an ungranted system path must still be refused")
	}
	if !strings.Contains(err.Error(), "oberth access allow") {
		t.Fatalf("the refusal must name the command that fixes it, got: %v", err)
	}

	granted := grantRequest(periapsis.TriggerRelease, map[string]bool{declared: true})
	if err := authorizeWithApprovalTable([]string{declared}, granted); err != nil {
		t.Fatalf("a granted system path must be admitted: %v", err)
	}
}

// TestCIStillCannotDeclareSystemPaths guards the other rule that shares this
// function: a branch run is bound to the ci-secrets identity, whose policy is
// structurally grant-free, so admitting a system declaration would produce a
// run that fails at Vault read time instead of at admission.
func TestCIStillCannotDeclareSystemPaths(t *testing.T) {
	t.Parallel()
	const declared = "oberth/data/release/cosign-secret"

	// Granted, so only the CI prohibition can be doing the refusing.
	request := grantRequest(periapsis.TriggerCI, map[string]bool{declared: true})
	err := authorizeWithApprovalTable([]string{declared}, request)
	if err == nil {
		t.Fatal("a CI run must not declare a system-namespace path even when granted")
	}
	if !strings.Contains(err.Error(), "system-namespace path") {
		t.Fatalf("the refusal must say why, got: %v", err)
	}
}

// TestReservedUpstreamSpellingIsStillRefused keeps the namespace unambiguous:
// the raw oberth/data/upstream/... spelling must not become a way to reach the
// hierarchical subtree through the system branch.
func TestReservedUpstreamSpellingIsStillRefused(t *testing.T) {
	t.Parallel()
	const declared = "oberth/data/upstream/skipops/github-token"
	request := grantRequest(periapsis.TriggerRelease, map[string]bool{declared: true})
	if err := authorizeWithApprovalTable([]string{declared}, request); err == nil {
		t.Fatal("the reserved raw spelling must be refused even with a grant")
	}
}
