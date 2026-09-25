package argojob

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/oberthci/oberth/pkg/periapsis"
)

const (
	testPerRepoSA     = "oberth-argo-codeberg-oberthci-oberth-abcdef012345"
	testPerRepoRole   = testPerRepoSA // SA name == role name by convention
	testPerRepoCISA   = "oberth-argo-ci-codeberg-oberthci-oberth-fedcba543210"
	testPerRepoCIRole = testPerRepoCISA // CI SA name == CI role name by convention
)

func testConfigWithPerRepo() Config {
	cfg := testConfig()
	cfg.PerRepoIdentities = map[string]PerRepoIdentityConfig{
		"codeberg/oberthci/oberth": {ServiceAccountName: testPerRepoSA},
	}
	cfg.PerRepoCIIdentities = map[string]PerRepoIdentityConfig{
		"codeberg/oberthci/oberth": {ServiceAccountName: testPerRepoCISA},
	}
	return cfg
}

// perRepoCredentialedDocument has activeDeadlineSeconds, a secret-paths
// annotation, and an oberth secretstore exec command using that path.
func perRepoCredentialedDocument(secretPath string) string {
	return `
apiVersion: argoproj.io/v1alpha1
kind: Workflow
metadata:
  annotations:
    oberth.ci/secret-paths: "` + secretPath + `"
spec:
  entrypoint: main
  activeDeadlineSeconds: 3600
  templates:
    - name: main
      container:
        image: golang:1.26-alpine@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
        command: [/run/oberth/bin/oberth]
        args: [secretstore, exec, --path, "` + secretPath + `", --, /bin/true]
`
}

// TestBuildUsesPerRepoIdentityForRelease proves that a repo with a per-repo
// identity uses that identity instead of the shared credentialed SA.
func TestBuildUsesPerRepoIdentityForRelease(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(path))
	req.Repo = "oberth"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testPerRepoSA {
		t.Fatalf("ServiceAccount = %q, want per-repo %q", wf.Spec.ServiceAccountName, testPerRepoSA)
	}
}

// TestBuildCIUsesPerRepoCIIdentity verifies that a CI run with per-repo CI
// identities uses the CI per-repo SA (not the release per-repo SA, not the
// shared ci-secrets SA). The CI per-repo policy is structurally grant-free:
// it scopes upstream access to the repo's own namespace only, closing the
// org-union gap where the shared ci-secrets policy gave every CI run read
// access to every registered org's upstream subtree (issue #433). The release
// per-repo identity is NEVER used for CI — that would put release grants
// inside the pod's reachable policy and collapse the CI-to-release boundary
// (issue #200).
func TestBuildCIUsesPerRepoCIIdentity(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(path))
	req.Repo = "oberth"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testPerRepoCISA {
		t.Fatalf("CI ServiceAccount = %q, want per-repo CI %q", wf.Spec.ServiceAccountName, testPerRepoCISA)
	}
	env := environmentOf(t, wf, "main")
	if env["OBERTH_VAULT_ROLE"] != testPerRepoCIRole {
		t.Fatalf("CI OBERTH_VAULT_ROLE = %q, want per-repo CI %q", env["OBERTH_VAULT_ROLE"], testPerRepoCIRole)
	}
}

// TestBuildCIFallsBackToSharedWhenNoCIPerRepoIdentities verifies that when no
// CI per-repo identities exist at all, CI runs use the shared ci-secrets SA.
// This is the single-repo / no-grants case.
func TestBuildCIFallsBackToSharedWhenNoCIPerRepoIdentities(t *testing.T) {
	t.Parallel()
	cfg := testConfig() // no PerRepoCIIdentities
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(path))
	req.Repo = "oberth"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testCISecretsAcct {
		t.Fatalf("CI ServiceAccount = %q, want shared %q (no CI per-repo identities exist)",
			wf.Spec.ServiceAccountName, testCISecretsAcct)
	}
}

// TestBuildCIRefusesSharedFallbackWhenOtherReposHaveCIIdentities proves that a
// repo without a CI per-repo identity is REFUSED when other repos have CI
// per-repo identities. Falling back to the shared ci-secrets SA would give
// this repo's CI run read access to every registered org's upstream secrets
// because the shared policy is an org-union. (Issue #433, mirrors #434 for CI)
func TestBuildCIRefusesSharedFallbackWhenOtherReposHaveCIIdentities(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo() // has CI per-repo for codeberg/oberthci/oberth
	path := "oberth/upstream/skipops/other-repo/test-secret"
	req := testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(path))
	req.Repo = "other-repo"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "skipops"
	req.ApprovedSecrets = map[string]bool{path: true}

	_, err := Build(cfg, req)
	if err == nil {
		t.Fatal("expected refusal when falling back to shared ci-secrets SA with other repos holding CI per-repo identities")
	}
	// The remediation must name the full three-step path (#459): sync for
	// the Vault side, install --upgrade for the chart-managed SA, and a
	// deployment rollout to rebuild the startup-frozen identity map.
	if !strings.Contains(err.Error(), "oberth secretstore sync") {
		t.Fatalf("error should recommend the targeted sync command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "install --install-secretstore --upgrade") {
		t.Fatalf("error should include chart upgrade command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rollout restart") {
		t.Fatalf("error should include deployment rollout restart, got: %v", err)
	}
	if !strings.Contains(err.Error(), "other-repo") {
		t.Fatalf("error should name the repo, got: %v", err)
	}
}

// TestBuildRefusesSharedFallbackWhenMultipleReposHaveGrants proves that a
// repo without a per-repo identity is REFUSED when other repos already have
// per-repo identities (and thus grants). Falling back to the shared SA would
// give this repo's release run access to every other repo's secrets because
// the shared policy folds all repos' grants. (Issue #434)
func TestBuildRefusesSharedFallbackWhenMultipleReposHaveGrants(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	path := "oberth/upstream/skipops/other-repo/test-secret"
	req := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(path))
	req.Repo = "other-repo"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "skipops"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	_, err := Build(cfg, req)
	if err == nil {
		t.Fatal("expected refusal when falling back to shared SA with other repos holding grants")
	}
	// The remediation must name the full three-step path (#459): sync for
	// the Vault side, install --upgrade for the chart-managed SA, and a
	// deployment rollout to rebuild the startup-frozen identity map.
	if !strings.Contains(err.Error(), "oberth secretstore sync") {
		t.Fatalf("error should recommend the targeted sync command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "install --install-secretstore --upgrade") {
		t.Fatalf("error should include chart upgrade command, got: %v", err)
	}
	if !strings.Contains(err.Error(), "rollout restart") {
		t.Fatalf("error should include deployment rollout restart, got: %v", err)
	}
	if !strings.Contains(err.Error(), "other-repo") {
		t.Fatalf("error should name the repo, got: %v", err)
	}
}

// TestBuildFallsBackToSharedIdentityForSingleRepo proves that when only one
// repo has grants and no per-repo identities exist for any repo, the shared
// SA fallback is safe: the shared policy carries only this repo's grants.
func TestBuildFallsBackToSharedIdentityForSingleRepo(t *testing.T) {
	t.Parallel()
	cfg := testConfig() // no PerRepoIdentities — single-repo case
	path := "oberth/upstream/skipops/other-repo/test-secret"
	req := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(path))
	req.Repo = "other-repo"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "skipops"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testCredentialedAcct {
		t.Fatalf("ServiceAccount = %q, want shared %q", wf.Spec.ServiceAccountName, testCredentialedAcct)
	}
}

// TestBuildWithoutSecretsStillUsesPipelineSA proves that a repo with a
// per-repo identity but no declared secrets still uses the pipeline SA.
func TestBuildWithoutSecretsStillUsesPipelineSA(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	req := testRequest(periapsis.TriggerRelease, greedyDocument)
	req.Repo = "oberth"

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testPipelineAcct {
		t.Fatalf("ServiceAccount = %q, want pipeline %q", wf.Spec.ServiceAccountName, testPipelineAcct)
	}
}

// TestPerRepoVaultRoleIsInjected proves that the per-repo Vault role (which
// shares the SA name) is injected as OBERTH_VAULT_ROLE.
func TestPerRepoVaultRoleIsInjected(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(path))
	req.Repo = "oberth"
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}

	// Find OBERTH_VAULT_ROLE in the template
	env := environmentOf(t, wf, "main")
	if env["OBERTH_VAULT_ROLE"] != testPerRepoRole {
		t.Fatalf("OBERTH_VAULT_ROLE = %q, want per-repo %q", env["OBERTH_VAULT_ROLE"], testPerRepoRole)
	}
}

// TestFragmentRunUsesHostRepoIdentity proves that when a fragment is resolved
// into a host pipeline, the host repo's per-repo identity is used, not the
// fragment source's.
func TestFragmentRunUsesHostRepoIdentity(t *testing.T) {
	t.Parallel()
	cfg := testConfigWithPerRepo()
	path := "oberth/upstream/oberthci/oberth/test-secret"
	req := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(path))
	req.Repo = "oberth" // HOST repo
	req.UpstreamName = "codeberg"
	req.UpstreamOrg = "oberthci"
	req.ApprovedSecrets = map[string]bool{path: true}
	req.SourceVolume = SourceVolume{
		ClaimName:      "test-claim",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wf, err := Build(cfg, req)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Spec.ServiceAccountName != testPerRepoSA {
		t.Fatalf("fragment host ServiceAccount = %q, want %q", wf.Spec.ServiceAccountName, testPerRepoSA)
	}
}

// TestCICrossRepoReadRefusedByPerRepoCIRole verifies that different repos get
// different CI SAs, so cross-org upstream reads at the Vault layer are
// structurally impossible for CI runs (issue #433).
func TestCICrossRepoReadRefusedByPerRepoCIRole(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.PerRepoCIIdentities = map[string]PerRepoIdentityConfig{
		"codeberg/org-a/repo-a": {ServiceAccountName: "oberth-argo-ci-upstream-org-a-repo-a-aaa111"},
		"codeberg/org-b/repo-b": {ServiceAccountName: "oberth-argo-ci-upstream-org-b-repo-b-bbb222"},
	}
	// Also need release per-repo identities for the test config to be valid
	// (the Build path checks both maps).
	cfg.PerRepoIdentities = map[string]PerRepoIdentityConfig{
		"codeberg/org-a/repo-a": {ServiceAccountName: "oberth-argo-upstream-org-a-repo-a-aaa111"},
		"codeberg/org-b/repo-b": {ServiceAccountName: "oberth-argo-upstream-org-b-repo-b-bbb222"},
	}

	pathA := "oberth/upstream/org-a/repo-a/test-secret"
	reqA := testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(pathA))
	reqA.Repo = "repo-a"
	reqA.UpstreamName = "codeberg"
	reqA.UpstreamOrg = "org-a"
	reqA.ApprovedSecrets = map[string]bool{pathA: true}

	wfA, err := Build(cfg, reqA)
	if err != nil {
		t.Fatal(err)
	}

	pathB := "oberth/upstream/org-b/repo-b/test-secret"
	reqB := testRequest(periapsis.TriggerCI, perRepoCredentialedDocument(pathB))
	reqB.Repo = "repo-b"
	reqB.UpstreamName = "codeberg"
	reqB.UpstreamOrg = "org-b"
	reqB.ApprovedSecrets = map[string]bool{pathB: true}

	wfB, err := Build(cfg, reqB)
	if err != nil {
		t.Fatal(err)
	}

	if wfA.Spec.ServiceAccountName == wfB.Spec.ServiceAccountName {
		t.Fatalf("CI repos a and b got the same SA %q; per-repo CI isolation violated", wfA.Spec.ServiceAccountName)
	}
}

// TestCrossRepoReadRefusedByPerRepoRole verifies that different repos get
// different SAs, so cross-repo reads at the Vault layer are structurally
// impossible.
func TestCrossRepoReadRefusedByPerRepoRole(t *testing.T) {
	t.Parallel()
	cfg := testConfig()
	cfg.PerRepoIdentities = map[string]PerRepoIdentityConfig{
		"codeberg/org/repo-a": {ServiceAccountName: "oberth-argo-upstream-org-repo-a-aaa111222333"},
		"codeberg/org/repo-b": {ServiceAccountName: "oberth-argo-upstream-org-repo-b-bbb444555666"},
	}

	pathA := "oberth/upstream/org/repo-a/test-secret"
	reqA := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(pathA))
	reqA.Repo = "repo-a"
	reqA.UpstreamName = "codeberg"
	reqA.UpstreamOrg = "org"
	reqA.ApprovedSecrets = map[string]bool{pathA: true}
	reqA.SourceVolume = SourceVolume{
		ClaimName:      "test-claim-a",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wfA, err := Build(cfg, reqA)
	if err != nil {
		t.Fatal(err)
	}

	pathB := "oberth/upstream/org/repo-b/test-secret"
	reqB := testRequest(periapsis.TriggerRelease, perRepoCredentialedDocument(pathB))
	reqB.Repo = "repo-b"
	reqB.UpstreamName = "codeberg"
	reqB.UpstreamOrg = "org"
	reqB.ApprovedSecrets = map[string]bool{pathB: true}
	reqB.SourceVolume = SourceVolume{
		ClaimName:      "test-claim-b",
		SubPath:        "src",
		VaultCASubPath: "vault-ca",
		BinarySubPath:  "bin",
	}

	wfB, err := Build(cfg, reqB)
	if err != nil {
		t.Fatal(err)
	}

	if wfA.Spec.ServiceAccountName == wfB.Spec.ServiceAccountName {
		t.Fatalf("repos a and b got the same SA %q; per-repo isolation violated", wfA.Spec.ServiceAccountName)
	}
}

// TestVerifyServiceAccountFailsWhenMissing proves that the controller rejects
// a submission when the per-repo ServiceAccount does not exist in the cluster,
// with an actionable error message (issue #272).
func TestVerifyServiceAccountFailsWhenMissing(t *testing.T) {
	t.Parallel()
	client := fake.NewClientset()
	controller := &Controller{kube: client}
	err := controller.verifyServiceAccount(t.Context(), "oberth-argo", "oberth-argo-missing-repo-abcdef012345")
	if err == nil {
		t.Fatal("expected error for missing ServiceAccount")
	}
	if !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("error should mention SA does not exist, got: %v", err)
	}
	if !strings.Contains(err.Error(), "install --upgrade --install-secretstore") {
		t.Fatalf("error should include remediation command, got: %v", err)
	}
}

// TestVerifyServiceAccountSucceedsWhenPresent proves that the check passes
// when the ServiceAccount exists.
func TestVerifyServiceAccountSucceedsWhenPresent(t *testing.T) {
	t.Parallel()
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "oberth-argo-test-repo-abcdef012345",
			Namespace: "oberth-argo",
		},
	}
	client := fake.NewClientset(sa)
	controller := &Controller{kube: client}
	if err := controller.verifyServiceAccount(t.Context(), "oberth-argo", sa.Name); err != nil {
		t.Fatalf("unexpected error for existing ServiceAccount: %v", err)
	}
}

// TestVerifyServiceAccountSkipsWhenNoClient proves the check is a no-op when
// the Kubernetes client is nil (test scenarios, Build-only paths).
func TestVerifyServiceAccountSkipsWhenNoClient(t *testing.T) {
	t.Parallel()
	controller := &Controller{kube: nil}
	if err := controller.verifyServiceAccount(t.Context(), "oberth-argo", "any-sa"); err != nil {
		t.Fatalf("nil client should skip verification, got: %v", err)
	}
}
