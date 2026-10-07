package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/identityname"
	"github.com/oberthci/oberth/internal/installer"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// testService creates a minimal API instance with an auditor and secret access
// store for secretstore_plan and secretstore_sync_receipt tests.
func testService(t *testing.T) *API {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	database, err := store.Open(ctx, filepath.Join(root, "oberth.sqlite"), store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.Close() })
	upstream, err := database.CreateUpstream(ctx, model.UpstreamSpec{
		Name: "codeberg", Kind: "ssh", BaseURL: "ssh://codeberg.org/acme",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = database.CreateRepository(ctx, model.RepositorySpec{
		Name: "oberth", UpstreamID: upstream.ID, DefaultBranch: "main",
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewAPI(APIConfig{
		Runs:         database,
		Auditor:      database,
		SecretAccess: database,
		MutationGate: func(context.Context) error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestSecretStorePlanFirstGrant(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin@tuxbox", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(result.Repos))
	}
	if result.Repos[0].Repo != "codeberg/acme/terraform" {
		t.Fatalf("unexpected repo: %s", result.Repos[0].Repo)
	}
	if len(result.Repos[0].Paths) != 1 || result.Repos[0].Paths[0] != "terraform/credentials" {
		t.Fatalf("unexpected paths: %v", result.Repos[0].Paths)
	}
	if result.Repos[0].Policy == "" {
		t.Fatal("policy name should be non-empty")
	}
	if !strings.Contains(result.Text, "terraform/credentials") {
		t.Fatalf("text should mention the path: %s", result.Text)
	}
}

func TestSecretStorePlanIdempotent(t *testing.T) {
	// No active grants -> empty plan.
	result := computeSecretStorePlan(nil)
	if len(result.Repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(result.Repos))
	}
	if !strings.Contains(result.Text, "No active grants") {
		t.Fatalf("text should mention no active grants: %s", result.Text)
	}
}

func TestSecretStorePlanExcludesRevoked(t *testing.T) {
	now := time.Now()
	revoked := now.Add(-time.Hour)
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now, RevokedAt: &revoked, RevokedBy: "admin"},
		{ID: 2, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/plan", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(result.Repos))
	}
	if len(result.Repos[0].Paths) != 1 {
		t.Fatalf("expected 1 path (revoked excluded), got %d", len(result.Repos[0].Paths))
	}
	if result.Repos[0].Paths[0] != "terraform/plan" {
		t.Fatalf("unexpected path: %s", result.Repos[0].Paths[0])
	}
}

func TestSecretStorePlanMultipleRepos(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/oberth", Step: "*", Secret: "cosign-secret", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 3, Repo: "codeberg/acme/oberth", Step: "*", Secret: "r2-upload", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(result.Repos))
	}
	// Repos should be sorted.
	if result.Repos[0].Repo != "codeberg/acme/oberth" {
		t.Fatalf("repos should be sorted, first is: %s", result.Repos[0].Repo)
	}
	if len(result.Repos[0].Paths) != 2 {
		t.Fatalf("oberth should have 2 paths, got %d", len(result.Repos[0].Paths))
	}
}

func TestSecretStorePlanDerivesPolicyName(t *testing.T) {
	name := derivePolicyName("codeberg/acme/terraform")
	if name != "oberth-argo-codeberg-acme-terraform" {
		t.Fatalf("unexpected policy name: %s", name)
	}
}

func TestSecretStorePlanDigestDeterministic(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/oberth", Step: "*", Secret: "cosign-secret", ApprovedBy: "admin", ApprovedAt: now},
	}
	result1 := computeSecretStorePlan(grants)
	result2 := computeSecretStorePlan(grants)
	if result1.Digest == "" {
		t.Fatal("digest should not be empty")
	}
	if result1.Digest != result2.Digest {
		t.Fatalf("digest is not deterministic: %s != %s", result1.Digest, result2.Digest)
	}
	if !strings.Contains(result1.Text, "digest:") {
		t.Fatalf("text should contain the digest: %s", result1.Text)
	}
}

func TestSecretStorePlanDigestChangesWithGrants(t *testing.T) {
	now := time.Now()
	grants1 := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now},
	}
	grants2 := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/plan", ApprovedBy: "admin", ApprovedAt: now},
	}
	result1 := computeSecretStorePlan(grants1)
	result2 := computeSecretStorePlan(grants2)
	if result1.Digest == result2.Digest {
		t.Fatal("digest should change when grants change")
	}
}

// --- Per-step plan tests (issue #623) ---

func TestSecretStorePlanPerStepEntries(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/port", Step: "*", Secret: "upstream/acme/port/config", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/port", Step: "release-publish-images", Secret: "release/gar-image-key", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 3, Repo: "codeberg/acme/port", Step: "release-publish-r2", Secret: "release/r2-token", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(result.Repos))
	}
	repo := result.Repos[0]
	if len(repo.Steps) != 2 {
		t.Fatalf("expected 2 per-step entries, got %d", len(repo.Steps))
	}
	// Steps should be sorted.
	if repo.Steps[0].Step != "release-publish-images" {
		t.Fatalf("expected first step 'release-publish-images', got %q", repo.Steps[0].Step)
	}
	if repo.Steps[1].Step != "release-publish-r2" {
		t.Fatalf("expected second step 'release-publish-r2', got %q", repo.Steps[1].Step)
	}
	// Each step should have its named path PLUS the wildcard-inherited path.
	for _, step := range repo.Steps {
		if len(step.Paths) != 2 {
			t.Fatalf("step %q: expected 2 paths (named + inherited wildcard), got %d: %v",
				step.Step, len(step.Paths), step.Paths)
		}
	}
}

func TestSecretStorePlanWildcardOnlyNoStepEntries(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/terraform", Step: "*", Secret: "terraform/credentials", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(result.Repos))
	}
	if len(result.Repos[0].Steps) != 0 {
		t.Fatalf("wildcard-only grants should produce no per-step entries, got %d", len(result.Repos[0].Steps))
	}
}

func TestSecretStorePlanPerStepPolicyName(t *testing.T) {
	// The plan's step policy name must equal the installer's PerStepName.
	// Issue #623, finding 3: a mismatch means the plan shows one name and
	// the installer creates a different one, making the admin-facing output
	// misleading and the plan digest wrong.
	planName := deriveStepPolicyName("codeberg/acme/port", "release-publish-images")
	installerName := installer.PerStepName("codeberg", "acme", "port", "release-publish-images")
	if planName != installerName {
		t.Fatalf("plan step name %q != installer step name %q", planName, installerName)
	}
	// Also verify the shared package produces the same.
	sharedName := identityname.PerStepName("codeberg", "acme", "port", "release-publish-images")
	if planName != sharedName {
		t.Fatalf("plan step name %q != identityname step name %q", planName, sharedName)
	}
}

func TestSecretStorePlanDigestChangesWithStepGrants(t *testing.T) {
	now := time.Now()
	// Baseline: wildcard only.
	grants1 := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/port", Step: "*", Secret: "release/key", ApprovedBy: "admin", ApprovedAt: now},
	}
	// Named step grant added.
	grants2 := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/port", Step: "*", Secret: "release/key", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/port", Step: "release-publish", Secret: "release/image-key", ApprovedBy: "admin", ApprovedAt: now},
	}
	result1 := computeSecretStorePlan(grants1)
	result2 := computeSecretStorePlan(grants2)
	if result1.Digest == result2.Digest {
		t.Fatal("digest should change when a per-step grant is added")
	}
}

func TestSecretStorePlanTextIncludesStepEntries(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/port", Step: "release-publish", Secret: "release/key", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if !strings.Contains(result.Text, "step: release-publish") {
		t.Fatalf("plan text should mention the step: %s", result.Text)
	}
}

func TestSecretStoreSyncReceiptNoSecretValues(t *testing.T) {
	// Verify that the audit details JSON from a sync receipt contains no
	// secret values — only names, digests, and counts.
	details := syncReceiptAuditDetails{
		PlanDigest:   "abc123",
		Policies:     map[string]bool{"credentialed policy": false, "per-repo policy abc": true},
		ChangedCount: 1,
		TotalCount:   2,
		Status:       "current",
	}
	raw, err := json.Marshal(details)
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	// These are patterns that would indicate secret leakage.
	for _, forbidden := range []string{"BAO_TOKEN", "VAULT_TOKEN", "root", "password", "secret_value"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("audit details contain forbidden pattern %q: %s", forbidden, text)
		}
	}
	// Verify it's valid JSON and has the expected fields.
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	if _, ok := parsed["plan_digest"]; !ok {
		t.Fatal("missing plan_digest in audit details")
	}
	if _, ok := parsed["status"]; !ok {
		t.Fatal("missing status in audit details")
	}
}

func TestSecretStoreSyncReceiptAdminRequired(t *testing.T) {
	service := testService(t)
	nonAdmin := api.Actor{Identity: "user@host", Admin: false}
	_, err := service.CallTool(context.Background(), nonAdmin, "secretstore_sync_receipt",
		json.RawMessage(`{"plan_digest":"abc123"}`))
	if err == nil {
		t.Fatal("expected error for non-admin uplink")
	}
	if !strings.Contains(err.Error(), "forbidden") {
		t.Fatalf("error should mention forbidden: %v", err)
	}
}

func TestSecretStoreSyncReceiptRecordsAuditAction(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	// First compute the current plan digest.
	planResult, err := service.CallTool(context.Background(), admin, "secretstore_plan", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	planResponse, ok := planResult.(SecretStorePlanResponse)
	if !ok {
		t.Fatalf("unexpected type: %T", planResult)
	}

	// Post the receipt with the matching digest -> should be "current".
	receiptJSON, _ := json.Marshal(map[string]any{
		"plan_digest":   planResponse.Digest,
		"changed_count": 0,
		"total_count":   0,
	})
	result, err := service.CallTool(context.Background(), admin, "secretstore_sync_receipt", json.RawMessage(receiptJSON))
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(SecretStoreSyncReceiptResponse)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if receipt.Status != "current" {
		t.Fatalf("expected current, got %s", receipt.Status)
	}
}

func TestSecretStoreSyncReceiptStalePlan(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	// Post a receipt with a mismatched digest -> should be "stale".
	receiptJSON := json.RawMessage(`{"plan_digest":"deadbeef","changed_count":1,"total_count":2}`)
	result, err := service.CallTool(context.Background(), admin, "secretstore_sync_receipt", receiptJSON)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(SecretStoreSyncReceiptResponse)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if receipt.Status != "stale" {
		t.Fatalf("expected stale, got %s", receipt.Status)
	}
}

func TestSecretStorePlanLastMaterializedNever(t *testing.T) {
	// When no sync receipt has been recorded, the tool must render
	// "last_materialized: never" in the text and set status "never"
	// in the structured response (#712).
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	result, err := service.CallTool(context.Background(), admin, "secretstore_plan", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	planResponse, ok := result.(SecretStorePlanResponse)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}

	// Structured: LastMaterialized must be non-nil with status "never".
	if planResponse.LastMaterialized == nil {
		t.Fatal("LastMaterialized must not be nil for the never-synced state")
	}
	if planResponse.LastMaterialized.Status != "never" {
		t.Fatalf("expected status never, got %q", planResponse.LastMaterialized.Status)
	}

	// Text: must contain exactly one "last_materialized: never" line.
	if count := strings.Count(planResponse.Text, "last_materialized: never"); count != 1 {
		t.Fatalf("expected exactly 1 'last_materialized: never' line, got %d in:\n%s", count, planResponse.Text)
	}

	// JSON round-trip: last_materialized must not be omitted.
	raw, err := json.Marshal(planResponse)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	lm, ok := parsed["last_materialized"]
	if !ok {
		t.Fatal("last_materialized must be present in JSON (not hidden by omitempty)")
	}
	lmMap, ok := lm.(map[string]any)
	if !ok {
		t.Fatalf("last_materialized should be an object, got %T", lm)
	}
	if lmMap["status"] != "never" {
		t.Fatalf("JSON status should be never, got %v", lmMap["status"])
	}
}

func TestSecretStorePlanLastMaterializedCurrent(t *testing.T) {
	// After recording a sync receipt with the matching digest, the tool
	// must render last_materialized with status "current" (#712).
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	// Compute current plan digest.
	result, err := service.CallTool(context.Background(), admin, "secretstore_plan", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	planResponse := result.(SecretStorePlanResponse)

	// Record a receipt with matching digest.
	receiptJSON, _ := json.Marshal(map[string]any{
		"plan_digest":   planResponse.Digest,
		"changed_count": 0,
		"total_count":   0,
	})
	_, err = service.CallTool(context.Background(), admin, "secretstore_sync_receipt", json.RawMessage(receiptJSON))
	if err != nil {
		t.Fatal(err)
	}

	// Re-query the plan — now it should show current.
	result, err = service.CallTool(context.Background(), admin, "secretstore_plan", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	planResponse = result.(SecretStorePlanResponse)

	if planResponse.LastMaterialized == nil {
		t.Fatal("LastMaterialized must not be nil after recording a receipt")
	}
	if planResponse.LastMaterialized.Status != "current" {
		t.Fatalf("expected status current, got %q", planResponse.LastMaterialized.Status)
	}
	if planResponse.LastMaterialized.Uplink != "admin@host" {
		t.Fatalf("expected uplink admin@host, got %q", planResponse.LastMaterialized.Uplink)
	}
	if planResponse.LastMaterialized.PlanDigest != planResponse.Digest {
		t.Fatalf("plan_digest mismatch: materialized %q vs current %q",
			planResponse.LastMaterialized.PlanDigest, planResponse.Digest)
	}

	// Text rendering must include the status=current line.
	expected := "last_materialized: " + planResponse.LastMaterialized.Time +
		" by admin@host plan_digest=" + planResponse.Digest + " status=current"
	if !strings.Contains(planResponse.Text, expected) {
		t.Fatalf("text should contain %q, got:\n%s", expected, planResponse.Text)
	}
	// Must not contain the "never" line.
	if strings.Contains(planResponse.Text, "last_materialized: never") {
		t.Fatalf("text should not contain 'last_materialized: never' after recording a receipt:\n%s", planResponse.Text)
	}
}

// --- access_list --pending tests (issue #623, finding 4) ---

func TestAccessListPendingDeclaredNotGranted(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	// Record a grant declaration (normally done at Workflow admission).
	ctx := context.Background()
	err := service.secretAccess.(*store.Store).RecordGrantDeclarations(ctx, "codeberg/acme/oberth", "abc123", []store.GrantDeclaration{
		{Step: "release-publish-images", Path: "oberth/data/release/gar-image-key"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Query pending — should return the declaration since no grant exists.
	result, err := service.CallTool(ctx, admin, "access_list",
		json.RawMessage(`{"pending":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pending, ok := result.(api.AccessListPendingResponse)
	if !ok {
		t.Fatalf("unexpected type: %T", result)
	}
	if len(pending.Declarations) != 1 {
		t.Fatalf("expected 1 pending declaration, got %d", len(pending.Declarations))
	}
	if pending.Declarations[0].Step != "release-publish-images" {
		t.Fatalf("unexpected step: %s", pending.Declarations[0].Step)
	}
}

func TestAccessListPendingGrantedNotPending(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}
	ctx := context.Background()

	db := service.secretAccess.(*store.Store)

	// Record a declaration.
	err := db.RecordGrantDeclarations(ctx, "codeberg/acme/oberth", "abc123", []store.GrantDeclaration{
		{Step: "release-publish-images", Path: "oberth/data/release/gar-image-key"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grant the exact (repo, step, path).
	_, err = db.Grant(ctx, "codeberg/acme/oberth", "release-publish-images", "oberth/data/release/gar-image-key", "admin@host")
	if err != nil {
		t.Fatal(err)
	}

	// Query pending — should be empty since the grant covers the declaration.
	result, err := service.CallTool(ctx, admin, "access_list",
		json.RawMessage(`{"pending":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pending := result.(api.AccessListPendingResponse)
	if len(pending.Declarations) != 0 {
		t.Fatalf("expected 0 pending declarations after grant, got %d", len(pending.Declarations))
	}
}

func TestAccessListPendingWildcardGrantCoversNamedDeclaration(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}
	ctx := context.Background()

	db := service.secretAccess.(*store.Store)

	// Record a named-step declaration.
	err := db.RecordGrantDeclarations(ctx, "codeberg/acme/oberth", "abc123", []store.GrantDeclaration{
		{Step: "release-publish-images", Path: "oberth/data/release/gar-image-key"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grant with wildcard step — should cover the named declaration.
	_, err = db.Grant(ctx, "codeberg/acme/oberth", "*", "oberth/data/release/gar-image-key", "admin@host")
	if err != nil {
		t.Fatal(err)
	}

	result, err := service.CallTool(ctx, admin, "access_list",
		json.RawMessage(`{"pending":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pending := result.(api.AccessListPendingResponse)
	if len(pending.Declarations) != 0 {
		t.Fatalf("expected wildcard grant to cover named declaration, got %d pending", len(pending.Declarations))
	}
}

func TestAccessListPendingRevokedGrantRePends(t *testing.T) {
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}
	ctx := context.Background()

	db := service.secretAccess.(*store.Store)

	// Record a declaration.
	err := db.RecordGrantDeclarations(ctx, "codeberg/acme/oberth", "abc123", []store.GrantDeclaration{
		{Step: "release-publish-images", Path: "oberth/data/release/gar-image-key"},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Grant then revoke.
	_, err = db.Grant(ctx, "codeberg/acme/oberth", "release-publish-images", "oberth/data/release/gar-image-key", "admin@host")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Revoke(ctx, "codeberg/acme/oberth", "release-publish-images", "oberth/data/release/gar-image-key", "admin@host")
	if err != nil {
		t.Fatal(err)
	}

	// Should be pending again after revocation.
	result, err := service.CallTool(ctx, admin, "access_list",
		json.RawMessage(`{"pending":true}`))
	if err != nil {
		t.Fatal(err)
	}
	pending := result.(api.AccessListPendingResponse)
	if len(pending.Declarations) != 1 {
		t.Fatalf("expected 1 pending after revocation, got %d", len(pending.Declarations))
	}
}

func TestSecretStorePlanLastMaterializedStale(t *testing.T) {
	// After recording a receipt with a mismatched digest, the tool must
	// render last_materialized with status "stale" (#712).
	service := testService(t)
	admin := api.Actor{Identity: "admin@host", Admin: true}

	// Record a receipt with a bogus digest.
	receiptJSON := json.RawMessage(`{"plan_digest":"deadbeef000000000000000000000000","changed_count":1,"total_count":3}`)
	_, err := service.CallTool(context.Background(), admin, "secretstore_sync_receipt", receiptJSON)
	if err != nil {
		t.Fatal(err)
	}

	// Query the plan — should show stale.
	result, err := service.CallTool(context.Background(), admin, "secretstore_plan", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	planResponse := result.(SecretStorePlanResponse)

	if planResponse.LastMaterialized == nil {
		t.Fatal("LastMaterialized must not be nil after recording a receipt")
	}
	if planResponse.LastMaterialized.Status != "stale" {
		t.Fatalf("expected status stale, got %q", planResponse.LastMaterialized.Status)
	}
	if planResponse.LastMaterialized.PlanDigest != "deadbeef000000000000000000000000" {
		t.Fatalf("plan_digest should be the recorded value, got %q", planResponse.LastMaterialized.PlanDigest)
	}

	// Text rendering must include status=stale.
	if !strings.Contains(planResponse.Text, "status=stale") {
		t.Fatalf("text should contain 'status=stale':\n%s", planResponse.Text)
	}
	// Must not contain the "never" line.
	if strings.Contains(planResponse.Text, "last_materialized: never") {
		t.Fatalf("text should not contain 'last_materialized: never' after recording a receipt:\n%s", planResponse.Text)
	}
}

// TestSecretStorePlanPerRepoExcludesNamedStepPaths: the plan the server
// prints must describe the same two path sets the sync materializes
// (issue #814): per-repo = wildcard grants only; per-step = named grants plus
// inherited wildcards. A named-step path listed under the repo policy would
// both misdescribe the live policy and desynchronize the plan digest from the
// sync receipt.
func TestSecretStorePlanPerRepoExcludesNamedStepPaths(t *testing.T) {
	now := time.Now()
	grants := []store.SecretAccessGrant{
		{ID: 1, Repo: "codeberg/acme/port", Step: "*", Secret: "release/cosign-secret", ApprovedBy: "admin", ApprovedAt: now},
		{ID: 2, Repo: "codeberg/acme/port", Step: "release-publish-images", Secret: "release/gar-image-key", ApprovedBy: "admin", ApprovedAt: now},
	}
	result := computeSecretStorePlan(grants)
	if len(result.Repos) != 1 {
		t.Fatalf("expected 1 repo, got %d", len(result.Repos))
	}
	repo := result.Repos[0]
	if got := strings.Join(repo.Paths, ","); got != "release/cosign-secret" {
		t.Fatalf("per-repo plan paths must be the wildcard grants only, got %q", got)
	}
	if len(repo.Steps) != 1 {
		t.Fatalf("expected 1 per-step entry, got %d", len(repo.Steps))
	}
	if got := strings.Join(repo.Steps[0].Paths, ","); got != "release/cosign-secret,release/gar-image-key" {
		t.Fatalf("per-step plan paths must be named ∪ wildcard, got %q", got)
	}
	// The text form lists the named path under the step only.
	repoBlock := result.Text[:strings.Index(result.Text, "  step: ")]
	if strings.Contains(repoBlock, "gar-image-key") {
		t.Fatalf("plan text lists the named-step path under the repo policy:\n%s", result.Text)
	}

	// A repo whose grants are all named keeps its per-repo entry (its
	// ServiceAccount and zero-grant policy still exist) with no paths.
	namedOnly := computeSecretStorePlan([]store.SecretAccessGrant{
		{ID: 3, Repo: "codeberg/acme/cttv", Step: "release-publish-images", Secret: "release/gar-image-key", ApprovedBy: "admin", ApprovedAt: now},
	})
	if len(namedOnly.Repos) != 1 || len(namedOnly.Repos[0].Paths) != 0 || len(namedOnly.Repos[0].Steps) != 1 {
		t.Fatalf("named-only repo must keep a per-repo entry with no paths and one step: %+v", namedOnly.Repos)
	}
}
