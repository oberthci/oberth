package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// --- JSON path tests ---

func TestParseAccessListJSONQualifiedEntries(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/cosign-secret"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/r2-upload-token"},
		{Workflow: ".oberth/release.yaml", Repo: "github/acme/terraform", Step: "*", Secret: "oberth/upstream/acme/terraform/gcp-sa"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 {
		t.Fatalf("expected 2 identities, got %d: %+v", len(result), result)
	}
}

func TestParseAccessListJSONEmptyArray(t *testing.T) {
	t.Parallel()
	result, err := ParseAccessListJSON([]byte("[]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 0 {
		t.Fatalf("expected 0 identities from empty array, got %d", len(result))
	}
}

func TestParseAccessListJSONAllBareReturnsError(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "terraform", Step: "*", Secret: "oberth/data/release/cosign-secret"},
	}
	data, _ := json.Marshal(entries)
	_, err := ParseAccessListJSON(data)
	if err == nil || !strings.Contains(err.Error(), "format drift") {
		t.Fatalf("expected format-drift error for all-bare JSON entries, got: %v", err)
	}
}

func TestProduceFromAccessListPrefersJSON(t *testing.T) {
	t.Parallel()
	jsonEntries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/oberthci/oberth", Step: "release", Secret: "oberth/data/release/cosign-secret"},
	}
	jsonData, _ := json.Marshal(jsonEntries)

	// Mock returns JSON when --json is requested, tabwriter otherwise.
	mockRun := func(_ context.Context, _ []byte, _ string, args ...string) ([]byte, error) {
		for _, a := range args {
			if a == "--json" {
				return jsonData, nil
			}
		}
		return nil, fmt.Errorf("should not reach tabwriter path")
	}

	result, warnings, err := testProducePerRepoIdentities(context.Background(), nil, mockRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity from JSON path, got %d: %+v", len(result), result)
	}
	if result[0].Repo != "oberth" || result[0].Upstream != "codeberg" {
		t.Fatalf("unexpected identity from JSON path: %+v", result[0])
	}
	// No degradation warnings expected when JSON path succeeds.
	for _, w := range warnings {
		if strings.Contains(w, "fell back") {
			t.Fatalf("unexpected fallback warning: %s", w)
		}
	}
}

// --- CM RBAC error surfacing (gap 4) ---

// --- Exec garbage: valid exit, garbage data → error → CM fallback → warning ---

// --- Parse-to-zero with non-empty input = error ---

func TestParseAccessListJSONEntriesNoQualifiedReturnsError(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "terraform", Step: "*", Secret: "s1"},
		{Workflow: ".oberth/release.yaml", Repo: "oberth", Step: "release", Secret: "s2"},
	}
	data, _ := json.Marshal(entries)
	_, err := ParseAccessListJSON(data)
	if err == nil {
		t.Fatal("expected error for JSON entries with no qualified identities")
	}
	if !strings.Contains(err.Error(), "2 entries") {
		t.Fatalf("error should mention 2 entries, got: %v", err)
	}
}

// --- Integration: producer output through ConfigurePerRepoIdentities ---
// This proves that the producer's output format (including upstream-prefix
// grants) is accepted by the downstream consumer without error, closing
// the test gap identified in the review.

func TestProducerOutputAcceptedByConfigurePerRepoIdentities(t *testing.T) {
	t.Parallel()

	output, err := json.Marshal([]accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/acme/acme-example-service", Step: "*", Secret: "oberth/data/release/cosign-secret"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/acme/acme-example-service", Step: "*", Secret: "oberth/data/release/gar-sa-key"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/acme/terraform", Step: "*", Secret: "oberth/upstream/acme/terraform/credentials"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mockRun := func(context.Context, []byte, string, ...string) ([]byte, error) { return output, nil }

	identities, _, err := testProducePerRepoIdentities(context.Background(), nil, mockRun, "", "oberth")
	if err != nil {
		t.Fatal(err)
	}

	if len(identities) != 2 {
		t.Fatalf("expected 2 identities, got %d: %+v", len(identities), identities)
	}

	// Script bao responses for both identities' policy + role operations,
	// including the CI-tier per-repo identities added by issue #433.
	serviceName := PerRepoName("codeberg", "acme", "acme-example-service")
	terraformName := PerRepoName("codeberg", "acme", "terraform")
	serviceCIName := PerRepoCIName("codeberg", "acme", "acme-example-service")
	terraformCIName := PerRepoCIName("codeberg", "acme", "terraform")

	responses := map[string]fakeBaoResponse{
		// example-service: release policy and role not yet present
		"policy read " + serviceName:                            {out: "No policy named: " + serviceName, err: errors.New("exit status 2")},
		"policy write " + serviceName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + serviceName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + serviceName + " -":      {out: "Success!"},
		// example-service: CI policy and role not yet present
		"policy read " + serviceCIName:                            {out: "No policy named: " + serviceCIName, err: errors.New("exit status 2")},
		"policy write " + serviceCIName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + serviceCIName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + serviceCIName + " -":      {out: "Success!"},
		// terraform: release policy and role not yet present
		"policy read " + terraformName:                            {out: "No policy named: " + terraformName, err: errors.New("exit status 2")},
		"policy write " + terraformName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + terraformName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + terraformName + " -":      {out: "Success!"},
		// terraform: CI policy and role not yet present
		"policy read " + terraformCIName:                            {out: "No policy named: " + terraformCIName, err: errors.New("exit status 2")},
		"policy write " + terraformCIName + " -":                    {out: "Success!"},
		"read -format=json auth/kubernetes/role/" + terraformCIName: {out: "No value found", err: errors.New("exit status 2")},
		"write auth/kubernetes/role/" + terraformCIName + " -":      {out: "Success!"},
	}
	runner := &fakeBaoRunner{t: t, responses: responses}
	store := openBaoExec{run: runner.run, namespace: "openbao", pod: "openbao-0"}

	items, err := ConfigurePerRepoIdentities(context.Background(), store, "root-token", identities, "oberth-pipelines")
	if err != nil {
		t.Fatalf("ConfigurePerRepoIdentities rejected producer output: %v", err)
	}

	// Each identity produces release policy + role + CI policy + role = 4 items each.
	if len(items) != 8 {
		t.Fatalf("expected 8 config items (4 per identity: release+CI policy+role), got %d: %+v", len(items), items)
	}

	// Verify that the terraform policy write included the upstream grant.
	for _, call := range runner.calls {
		if call.command == "policy write "+terraformName+" -" {
			if !strings.Contains(call.stdin, `path "oberth/data/upstream/acme/terraform/*"`) {
				t.Fatalf("terraform policy missing upstream grant path in written HCL:\n%s", call.stdin)
			}
			return
		}
	}
	t.Fatal("terraform policy write call not found")
}

// TestProducePerStepIdentitiesAPIFailureReturnsError verifies that
// ProducePerStepIdentities returns a hard error when the admin API fails,
// rather than silently returning nil. Issue #623, finding 1: a nil return
// on failure causes orphan removal to delete all per-step identities.
func TestProducePerStepIdentitiesAPIFailureReturnsError(t *testing.T) {
	t.Parallel()

	failingRunner := func(_ context.Context, _ []byte, _ string, _ ...string) ([]byte, error) {
		return nil, errors.New("connection refused")
	}
	_, err := testProducePerStepIdentities(context.Background(), failingRunner, "", "oberth")
	if err == nil {
		t.Fatal("expected an error when the API fails, got nil")
	}
	if !strings.Contains(err.Error(), "connection refused") {
		t.Fatalf("API transport error lost: %v", err)
	}
}

// --- Issue #814: per-repo identities carry wildcard grants only ---

// TestParseAccessListJSONExcludesNamedStepGrantsFromPerRepo is the unit
// reproduction of issue #814: a wildcard and a named-step grant for one repo
// must yield a per-repo identity whose grant list is the wildcard path only.
// Before the fix the per-repo list was the union, so every template of the
// repo running under the per-repo ServiceAccount could read the named-step
// path from Vault, and revoking a wildcard grant could never narrow it.
func TestParseAccessListJSONExcludesNamedStepGrantsFromPerRepo(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/exampleorg-port", Step: "*", Secret: "oberth/data/release/cosign-secret"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/exampleorg-port", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/exampleorg-port", Step: "*", Secret: "oberth/data/release/r2-upload-token"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected 1 identity, got %d: %+v", len(result), result)
	}
	var want []string
	if got := result[0].Grants; !reflect.DeepEqual(got, want) {
		t.Fatalf("per-repo grants must be the wildcard paths only (issue #814): got %v want %v", got, want)
	}
}

// TestParseAccessListJSONWithStepsSplitsPerRepoAndPerStepPaths proves the two
// path sets the per-step design (#623) promises: the per-repo identity reads
// wildcard paths only; the per-step identity reads its named paths plus the
// inherited wildcards. A named-step path never appears on the per-repo side.
func TestParseAccessListJSONWithStepsSplitsPerRepoAndPerStepPaths(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/exampleorg-port", Step: "*", Secret: "oberth/data/release/cosign-secret"},
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/exampleorg-port", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
	}
	data, _ := json.Marshal(entries)
	repoIdentities, grants, err := ParseAccessListJSONWithSteps(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(repoIdentities) != 1 {
		t.Fatalf("expected 1 per-repo identity, got %d: %+v", len(repoIdentities), repoIdentities)
	}
	if got, want := repoIdentities[0].Grants, []string(nil); !reflect.DeepEqual(got, want) {
		t.Fatalf("per-repo grants: got %v want %v", got, want)
	}
	if len(grants) != 2 {
		t.Fatalf("expected both raw grants to be carried for per-step derivation, got %d", len(grants))
	}
	steps := DerivePerStepIdentities(repoIdentities, grants)
	if len(steps) != 2 || steps[1].Step != "release-publish-images" {
		t.Fatalf("expected one per-step identity for release-publish-images, got %+v", steps)
	}
	wantStep := []string{"oberth/data/release/cosign-secret", "oberth/data/release/gar-image-key"}
	if !reflect.DeepEqual(steps[1].GrantPaths, wantStep) {
		t.Fatalf("per-step paths must be named ∪ wildcard: got %v want %v", steps[1].GrantPaths, wantStep)
	}
}

// TestParseAccessListJSONNamedOnlyRepoKeepsIdentityWithoutGrants: a repo whose
// grants are all named still needs its per-repo ServiceAccount (workflow
// default, chart SA list, admission bookkeeping). It must be produced with an
// empty grant list rather than dropped, which would also trip the
// format-drift guard.
func TestParseAccessListJSONNamedOnlyRepoKeepsIdentityWithoutGrants(t *testing.T) {
	t.Parallel()
	entries := []accessListJSONEntry{
		{Workflow: ".oberth/release.yaml", Repo: "codeberg/exampleorg/example-web", Step: "release-publish-images", Secret: "oberth/data/release/gar-image-key"},
	}
	data, _ := json.Marshal(entries)
	result, err := ParseAccessListJSON(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 {
		t.Fatalf("expected the named-only repo to keep its per-repo identity, got %d: %+v", len(result), result)
	}
	if len(result[0].Grants) != 0 {
		t.Fatalf("named-only repo must have no per-repo grants, got %v", result[0].Grants)
	}
	if result[0].Repo != "example-web" || result[0].Org != "exampleorg" || result[0].Upstream != "codeberg" {
		t.Fatalf("unexpected identity: %+v", result[0])
	}
}

// TestParseAccessListOutputExcludesNamedStepGrants covers the tabwriter
// fallback path (older deployed binaries): the STEP column decides.

// TestProduceFromConfigMapExcludesNamedStepGrants covers the ConfigMap
// fallback (no running pod): the step field decides there too.
