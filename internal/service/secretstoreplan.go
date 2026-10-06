package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/oberthci/oberth/internal/api"
	"github.com/oberthci/oberth/internal/model"
	"github.com/oberthci/oberth/internal/store"
)

// SecretStorePlanResponse describes the policy/role operations that
// `oberth secretstore sync` would apply based on the approved grants.
// Read-only: the server never receives an admin Bao token (#611).
type SecretStorePlanResponse struct {
	Repos            []SecretStorePlanRepo            `json:"repos"`
	Digest           string                           `json:"digest"`
	Text             string                           `json:"text"`
	LastMaterialized *SecretStorePlanLastMaterialized `json:"last_materialized"`
}

func (r SecretStorePlanResponse) MCPToolText() string  { return r.Text }
func (r SecretStorePlanResponse) MCPToolIsError() bool { return false }

// SecretStorePlanLastMaterialized describes the most recent secretstore.sync
// audit action. Status is "current" when the recorded plan digest matches the
// current plan, "stale" when they differ, or "never" when no sync has been
// recorded.
type SecretStorePlanLastMaterialized struct {
	Time       string `json:"time"`
	Uplink     string `json:"uplink"`
	PlanDigest string `json:"plan_digest"`
	Status     string `json:"status"` // "current", "stale", or "never"
}

// SecretStorePlanRepo describes the desired policy state for one repository.
type SecretStorePlanRepo struct {
	Repo   string                `json:"repo"`
	Paths  []string              `json:"paths"`
	Policy string                `json:"policy"`
	Steps  []SecretStorePlanStep `json:"steps,omitempty"`
}

// SecretStorePlanStep describes the desired per-step policy state for one
// (repo, step) combination. Issue #623.
type SecretStorePlanStep struct {
	Step   string   `json:"step"`
	Paths  []string `json:"paths"`
	Policy string   `json:"policy"`
}

func (service *API) secretStorePlan(ctx context.Context, actor api.Actor, raw json.RawMessage) (SecretStorePlanResponse, error) {
	if !actor.Admin {
		return SecretStorePlanResponse{}, fmt.Errorf("%w: secretstore_plan requires an admin uplink", ErrForbidden)
	}
	if service.secretAccess == nil {
		return SecretStorePlanResponse{}, fmt.Errorf("%w: secret access", ErrUnavailable)
	}
	// Read all active grants.
	grants, err := service.secretAccess.SecretAccessList(ctx, "", false)
	if err != nil {
		return SecretStorePlanResponse{}, err
	}
	response := computeSecretStorePlan(grants)

	// Look up the latest secretstore.sync audit action to populate last_materialized.
	// The "never" state is always rendered explicitly so an operator can
	// distinguish "never synced" from "server predates #712" (#712).
	materialized := &SecretStorePlanLastMaterialized{Status: "never"}
	if service.auditor != nil {
		if reader, ok := service.auditor.(AuditActionReader); ok {
			latest, readErr := reader.LatestAuditActionByType(ctx, "secretstore.sync", "secretstore")
			if readErr == nil {
				status := "current"
				var details syncReceiptAuditDetails
				if json.Unmarshal([]byte(latest.Details), &details) == nil && details.PlanDigest != response.Digest {
					status = "stale"
				}
				materialized = &SecretStorePlanLastMaterialized{
					Time:       latest.CreatedAt.UTC().Format("2006-01-02T15:04:05Z"),
					Uplink:     latest.Actor,
					PlanDigest: details.PlanDigest,
					Status:     status,
				}
			}
		}
	}
	response.LastMaterialized = materialized
	response.Text += "\n" + renderLastMaterialized(materialized)

	return response, nil
}

// computeSecretStorePlan groups grants by repo and computes the desired
// policy structure for each, including per-step entries for named grants
// (issue #623). This is a pure computation with no side effects and no
// secret values.
func computeSecretStorePlan(grants []store.SecretAccessGrant) SecretStorePlanResponse {
	// Group paths by repo and by (repo, step).
	repoPathsMap := make(map[string][]string)
	// stepPathsMap[repo][step] = []paths for named-step grants.
	stepPathsMap := make(map[string]map[string][]string)
	// wildcardPaths[repo] = []paths with step="*".
	wildcardPaths := make(map[string][]string)
	for _, grant := range grants {
		if grant.RevokedAt != nil {
			continue
		}
		repoPathsMap[grant.Repo] = append(repoPathsMap[grant.Repo], grant.Secret)
		if grant.Step == "*" {
			wildcardPaths[grant.Repo] = append(wildcardPaths[grant.Repo], grant.Secret)
		} else {
			if stepPathsMap[grant.Repo] == nil {
				stepPathsMap[grant.Repo] = make(map[string][]string)
			}
			stepPathsMap[grant.Repo][grant.Step] = append(
				stepPathsMap[grant.Repo][grant.Step], grant.Secret)
		}
	}
	// Sort repos for deterministic output.
	repos := make([]string, 0, len(repoPathsMap))
	for repo := range repoPathsMap {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	var result []SecretStorePlanRepo
	var lines []string
	if len(repos) == 0 {
		lines = append(lines, "No active grants. secretstore sync would produce zero-stanza shared policies and no per-repo policies.")
	}
	for _, repo := range repos {
		paths := repoPathsMap[repo]
		sort.Strings(paths)
		policyName := derivePolicyName(repo)
		entry := SecretStorePlanRepo{
			Repo:   repo,
			Paths:  paths,
			Policy: policyName,
		}

		// Per-step entries for named grants.
		if steps, ok := stepPathsMap[repo]; ok {
			stepNames := make([]string, 0, len(steps))
			for step := range steps {
				stepNames = append(stepNames, step)
			}
			sort.Strings(stepNames)
			for _, step := range stepNames {
				stepPaths := steps[step]
				// Merge wildcard-granted paths (inherited by all steps).
				merged := make([]string, 0, len(stepPaths)+len(wildcardPaths[repo]))
				merged = append(merged, stepPaths...)
				for _, wp := range wildcardPaths[repo] {
					found := false
					for _, sp := range merged {
						if sp == wp {
							found = true
							break
						}
					}
					if !found {
						merged = append(merged, wp)
					}
				}
				sort.Strings(merged)
				stepPolicyName := deriveStepPolicyName(repo, step)
				entry.Steps = append(entry.Steps, SecretStorePlanStep{
					Step:   step,
					Paths:  merged,
					Policy: stepPolicyName,
				})
			}
		}

		result = append(result, entry)
		lines = append(lines, fmt.Sprintf("repo: %s", repo))
		lines = append(lines, fmt.Sprintf("  policy: %s", policyName))
		for _, path := range paths {
			lines = append(lines, fmt.Sprintf("  path: %s", path))
		}
		for _, stepEntry := range entry.Steps {
			lines = append(lines, fmt.Sprintf("  step: %s", stepEntry.Step))
			lines = append(lines, fmt.Sprintf("    policy: %s", stepEntry.Policy))
			for _, path := range stepEntry.Paths {
				lines = append(lines, fmt.Sprintf("    path: %s", path))
			}
		}
	}
	digest := ComputePlanDigest(result)
	return SecretStorePlanResponse{
		Repos:  result,
		Digest: digest,
		Text:   strings.Join(lines, "\n") + "\ndigest: " + digest,
	}
}

// ComputePlanDigest computes a deterministic sha256 digest of the plan's
// canonical representation. The plan repos must already be sorted by repo
// name with each repo's paths sorted. This digest is the same value the
// secretstore_plan tool prints and the sync receipt records — comparing them
// answers "is the materialized state current?"
//
// v2: includes per-step entries in the digest (issue #623). The digest is
// backward compatible: repos with no steps produce the same bytes as the
// v1 format because the step loop simply doesn't write anything.
func ComputePlanDigest(repos []SecretStorePlanRepo) string {
	h := sha256.New()
	h.Write([]byte("oberth-secretstore-plan-v1\x00"))
	for _, repo := range repos {
		h.Write([]byte(repo.Repo))
		h.Write([]byte{0})
		h.Write([]byte(repo.Policy))
		h.Write([]byte{0})
		for _, path := range repo.Paths {
			h.Write([]byte(path))
			h.Write([]byte{0})
		}
		// Per-step entries (issue #623). Repos without steps produce no
		// additional bytes, preserving backward compatibility with v1 digests.
		for _, step := range repo.Steps {
			h.Write([]byte("step:"))
			h.Write([]byte(step.Step))
			h.Write([]byte{0})
			h.Write([]byte(step.Policy))
			h.Write([]byte{0})
			for _, path := range step.Paths {
				h.Write([]byte(path))
				h.Write([]byte{0})
			}
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// derivePolicyName converts a qualified repo identity (upstream/org/repo)
// into the conventional Vault policy name, matching the installer's naming.
func derivePolicyName(repo string) string {
	// Replace / with - and prepend "oberth-argo-"
	safe := strings.NewReplacer("/", "-", ".", "-").Replace(repo)
	return "oberth-argo-" + safe
}

// deriveStepPolicyName converts a qualified repo identity and step name into
// the conventional per-step Vault policy name. This mirrors the installer's
// PerStepName but uses the same simple derivation for the plan (the exact
// hash-suffixed name is computed at install/sync time by
// installer.PerStepName).
func deriveStepPolicyName(repo, step string) string {
	safe := strings.NewReplacer("/", "-", ".", "-").Replace(repo)
	return "oberth-step-" + safe + "-" + step
}

// renderLastMaterialized returns the text line for the last_materialized
// field. The "never" state is a single token; receipt states include the
// full provenance (time, uplink, plan_digest, status).
func renderLastMaterialized(m *SecretStorePlanLastMaterialized) string {
	if m.Status == "never" {
		return "last_materialized: never"
	}
	return fmt.Sprintf("last_materialized: %s by %s plan_digest=%s status=%s",
		m.Time, m.Uplink, m.PlanDigest, m.Status)
}

// syncReceiptAuditDetails is the JSON structure recorded in the audit chain's
// details field for a secretstore.sync action. It must never contain secret
// values — only the plan digest, policy names, and change counts.
type syncReceiptAuditDetails struct {
	PlanDigest   string          `json:"plan_digest"`
	Policies     map[string]bool `json:"policies"`
	ChangedCount int             `json:"changed_count"`
	TotalCount   int             `json:"total_count"`
	Status       string          `json:"status"` // "current" or "stale"
}

// SecretStoreSyncReceiptRequest is the input to the secretstore_sync_receipt
// MCP tool.
type SecretStoreSyncReceiptRequest struct {
	PlanDigest   string          `json:"plan_digest"`
	Policies     map[string]bool `json:"policies"`
	ChangedCount int             `json:"changed_count"`
	TotalCount   int             `json:"total_count"`
}

// SecretStoreSyncReceiptResponse is the MCP tool response.
type SecretStoreSyncReceiptResponse struct {
	Status string `json:"status"` // "current" or "stale"
	Text   string `json:"text"`
}

func (r SecretStoreSyncReceiptResponse) MCPToolText() string  { return r.Text }
func (r SecretStoreSyncReceiptResponse) MCPToolIsError() bool { return false }

// secretStoreSyncReceipt records a secretstore.sync audit action with the
// typed receipt posted by the CLI after a successful sync. It recomputes the
// plan digest from the current approvals and marks the receipt "current" when
// they match, "stale" when they differ. It never refuses to record — a stale
// receipt is still valuable audit evidence.
func (service *API) secretStoreSyncReceipt(ctx context.Context, actor api.Actor, raw json.RawMessage) (SecretStoreSyncReceiptResponse, error) {
	if !actor.Admin {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("%w: secretstore_sync_receipt requires an admin uplink", ErrForbidden)
	}
	if service.auditor == nil {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("%w: audit", ErrUnavailable)
	}
	if service.secretAccess == nil {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("%w: secret access", ErrUnavailable)
	}

	var request SecretStoreSyncReceiptRequest
	if err := decodeTool(raw, &request); err != nil {
		return SecretStoreSyncReceiptResponse{}, err
	}
	if request.PlanDigest == "" {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("%w: plan_digest is required", ErrInvalidInput)
	}

	// Recompute the current plan digest from the live approval table.
	grants, err := service.secretAccess.SecretAccessList(ctx, "", false)
	if err != nil {
		return SecretStoreSyncReceiptResponse{}, err
	}
	currentPlan := computeSecretStorePlan(grants)

	status := "current"
	if request.PlanDigest != currentPlan.Digest {
		status = "stale"
	}

	details := syncReceiptAuditDetails{
		PlanDigest:   request.PlanDigest,
		Policies:     request.Policies,
		ChangedCount: request.ChangedCount,
		TotalCount:   request.TotalCount,
		Status:       status,
	}
	detailsJSON, err := json.Marshal(details)
	if err != nil {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("encode sync receipt details: %w", err)
	}

	if _, err := service.auditor.AppendAuditAction(ctx, model.AuditActionSpec{
		Actor:        actor.Identity,
		Action:       "secretstore.sync",
		ResourceType: "secretstore",
		ResourceID:   request.PlanDigest,
		Details:      string(detailsJSON),
	}); err != nil {
		return SecretStoreSyncReceiptResponse{}, fmt.Errorf("record sync receipt: %w", err)
	}

	text := fmt.Sprintf("secretstore.sync recorded: status=%s plan_digest=%s changed=%d/%d",
		status, request.PlanDigest, request.ChangedCount, request.TotalCount)
	return SecretStoreSyncReceiptResponse{Status: status, Text: text}, nil
}
